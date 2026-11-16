package connect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Auth 是已验证 App 的授权快照，以 AuthorizationID 区分独立连接。
// origin 仅绑定实际传输来源；Generation 绑定签发时的后台运行代际。
// AppPublicKeyHex 必填，由 URL 验签或有效部署登记产生。
type Auth struct {
	DeploymentGeneration string `json:"deploymentGeneration,omitempty"`
	DeploymentID         string `json:"deploymentId,omitempty"`
	AuthorizationID      string `json:"authorizationId"`
	Origin               string `json:"origin"`
	Network              string `json:"network"`
	WalletPublicKey      string `json:"walletPublicKey"`
	Generation           string `json:"generation"`
	CreatedAt            int64  `json:"createdAt"`
	AppPublicKeyHex      string `json:"appPublicKeyHex"`
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

// Load 只读取完整的公钥授权快照；无效记录使整个加载失败。
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
		if err := validateAuth(a); err != nil {
			return err
		}
		if _, exists := m[a.AuthorizationID]; exists {
			return fmt.Errorf("duplicate authorizationId")
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

func validateAuth(a Auth) error {
	if ApplicationKey(a.AppPublicKeyHex) == "" {
		return fmt.Errorf("verified App public key required")
	}
	origin, err := CanonicalOrigin(a.Origin)
	if err != nil || origin != a.Origin || a.AuthorizationID == "" || a.Generation == "" || a.CreatedAt <= 0 || (a.Network != "test" && a.Network != "main") || ApplicationKey(a.WalletPublicKey) == "" {
		return fmt.Errorf("invalid authorization snapshot")
	}
	if (a.DeploymentID == "") != (a.DeploymentGeneration == "") {
		return fmt.Errorf("invalid deployment authorization snapshot")
	}
	return nil
}

func (s *AuthStore) Save(a Auth) error {
	if ApplicationKey(a.AppPublicKeyHex) == "" {
		return fmt.Errorf("verified App public key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.AuthorizationID == "" {
		return fmt.Errorf("缺少 authorizationId")
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().UnixMilli()
	}
	if err := validateAuth(a); err != nil {
		return err
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

// RevokeApp 仅撤销该公钥的连接授权，不删除许可与业务数据。
func (s *AuthStore) RevokeApp(key string) error {
	return s.revokeLocked(func(a Auth) bool { return a.AppPublicKeyHex == key }, true)
}
