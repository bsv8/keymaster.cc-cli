package connect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Auth 为 App 授权记录。以 AuthorizationID 为主键持久化；同一 App 在不同
// 来源各持一条授权，来源、网络、钱包、身份摘要任一变化即视为不同授权。
// 世代（Generation）绑定签发时的后台运行代际；后台重启后旧授权必须重新登录。
type Auth struct {
	AuthorizationID string   `json:"authorizationId"`
	AppPublicKey    string   `json:"appPublicKey"`
	AppID           string   `json:"appId"`
	AppName         string   `json:"appName"`
	IdentityDigest  string   `json:"identityDigestHex"`
	Requirements    []string `json:"requirements"`
	Origin          string   `json:"origin"`
	Network         string   `json:"network"`
	WalletPublicKey string   `json:"walletPublicKey"`
	Generation      string   `json:"generation"`
	CreatedAt       int64    `json:"createdAt"`
}

// AuthStore 持久保存授权（钱包根/connect/auth.json）。
type AuthStore struct {
	mu    sync.Mutex
	path  string
	auths map[string]Auth // key: AuthorizationID
}

func NewAuthStore(walletDir string) *AuthStore {
	return &AuthStore{path: filepath.Join(walletDir, "connect", "auth.json"), auths: map[string]Auth{}}
}

// Load 读取持久授权；兼容仅含旧字段的记录（缺失字段留空，调用方按代际与
// 摘要重新校验，旧记录自然失效，需重新登录）。
func (s *AuthStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []Auth
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	m := map[string]Auth{}
	for _, a := range list {
		if a.AuthorizationID == "" {
			// 旧格式：以 App 公钥为键迁移为单条记录，代际留空以便强制重新登录。
			a.AuthorizationID = "auth_" + a.AppPublicKey
		}
		m[a.AuthorizationID] = a
	}
	s.auths = m
	return nil
}

func (s *AuthStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	list := make([]Auth, 0, len(s.auths))
	for _, a := range s.auths {
		list = append(list, a)
	}
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *AuthStore) Save(a Auth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.AuthorizationID == "" {
		return fmt.Errorf("缺少 authorizationId")
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().UnixMilli()
	}
	prev, hadPrev := s.auths[a.AuthorizationID]
	s.auths[a.AuthorizationID] = a
	if err := s.saveLocked(); err != nil {
		if hadPrev {
			s.auths[a.AuthorizationID] = prev
		} else {
			delete(s.auths, a.AuthorizationID)
		}
		return err
	}
	return nil
}

func (s *AuthStore) List() []Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Auth, 0, len(s.auths))
	for _, a := range s.auths {
		out = append(out, a)
	}
	return out
}

// FindByID 按授权 ID 精确查找。
func (s *AuthStore) FindByID(id string) (Auth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.auths[id]
	return a, ok
}

// FindByApp 返回指定 App 公钥的全部授权（用于兼容查询与整体撤销）。
func (s *AuthStore) FindByApp(appPubKey string) []Auth {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Auth
	for _, a := range s.auths {
		if a.AppPublicKey == appPubKey {
			out = append(out, a)
		}
	}
	return out
}

// Revoke 保留仅供内部精确替换旧授权使用；对外 API 不再暴露按公钥粗粒度撤销。
// 新代码优先使用 RevokeExact，避免误撤同一发行人其他应用。
func (s *AuthStore) Revoke(appPubKey string) error {
	return s.revokeLocked(func(a Auth) bool { return a.AppPublicKey == appPubKey }, true)
}

func (s *AuthStore) revokeLocked(match func(Auth) bool, needFound bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []Auth
	for id, a := range s.auths {
		if match(a) {
			removed = append(removed, a)
			delete(s.auths, id)
		}
	}
	if needFound && len(removed) == 0 {
		return fmt.Errorf("授权不存在")
	}
	if err := s.saveLocked(); err != nil {
		for _, a := range removed {
			s.auths[a.AuthorizationID] = a
		}
		return err
	}
	return nil
}

// RevokeExact 按精确应用身份撤销该应用全部连接，不影响同发行人其他 App 或其他来源。
func (s *AuthStore) RevokeExact(origin, appPubKey, appID string) error {
	return s.revokeLocked(func(a Auth) bool {
		if a.Origin != origin {
			return false
		}
		if appPubKey != "" && !equalFold(a.AppPublicKey, appPubKey) {
			return false
		}
		if appID != "" && a.AppID != appID {
			return false
		}
		return true
	}, true)
}

// FindExact 按精确应用身份查找有效授权。

// RevokeByID 撤销单条授权；落盘失败回滚内存。
func (s *AuthStore) RevokeByID(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.auths[id]
	if !ok {
		return fmt.Errorf("授权不存在")
	}
	delete(s.auths, id)
	if err := s.saveLocked(); err != nil {
		s.auths[id] = prev
		return err
	}
	return nil
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
