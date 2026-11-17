package connect

import (
	"sync"
	"time"
)

type Connection struct {
	ownerID   string // 创建连接的钱包 WebSocket；只在本运行内使用。
	ID        string `json:"id"`
	AuthID    string `json:"authId"`
	AppKey    string `json:"appKey"`
	Origin    string `json:"origin"`
	AppName   string `json:"appName,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	LastSeen  int64  `json:"lastSeen"`
	ClosedAt  int64  `json:"closedAt,omitempty"`
}

// ConnectionStore 为内存运行态注册表（不跨重启；重启后旧代际连接整体失效，
// 由新登录/启动重建）。读写均经互斥锁；LastSeen 更新只动内存，不落盘。
type ConnectionStore struct {
	mu    sync.Mutex
	conns map[string]*Connection // key: AuthID
}

func NewConnectionStore() *ConnectionStore {
	return &ConnectionStore{conns: map[string]*Connection{}}
}

// UpsertActive 登记登录或启动创建的运行连接。
func (s *ConnectionStore) UpsertActive(auth Auth, ownerID string) Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if c, ok := s.conns[auth.AuthorizationID]; ok {
		c.AppKey = ApplicationKey(auth.AppPublicKeyHex)
		c.Origin = auth.Origin
		c.ownerID = ownerID
		c.LastSeen = now
		c.ClosedAt = 0
		return *c
	}
	c := &Connection{
		ID: auth.AuthorizationID, AuthID: auth.AuthorizationID, ownerID: ownerID,
		AppKey:    ApplicationKey(auth.AppPublicKeyHex),
		Origin:    auth.Origin,
		CreatedAt: now, LastSeen: now,
	}
	s.conns[auth.AuthorizationID] = c
	return *c
}

// Touch 更新活跃时间（每次鉴权成功的业务调用）；连接不存在返回 false。
// 已断开的连接不能通过业务调用恢复。
func (s *ConnectionStore) Touch(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[authID]
	if !ok || c.ClosedAt != 0 {
		return false
	}
	c.LastSeen = time.Now().UnixMilli()
	return true
}

// Close 关闭连接（退出/撤销时调用，保留记录供历史展示）。
func (s *ConnectionStore) Close(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[authID]
	if !ok || c.ClosedAt != 0 {
		return false
	}
	c.ClosedAt = time.Now().UnixMilli()
	return true
}

// CloseOwner 终结指定钱包通道的运行连接，不撤销长期应用许可或已提交业务。
func (s *ConnectionStore) CloseOwner(ownerID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, c := range s.conns {
		if c.ownerID == ownerID && c.ClosedAt == 0 {
			c.ClosedAt = time.Now().UnixMilli()
			count++
		}
	}
	return count
}

// CloseByApp 按 App 公钥关闭全部连接。
func (s *ConnectionStore) CloseByApp(appPublicKeyHex string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	n := 0
	for _, c := range s.conns {
		if c.ClosedAt != 0 {
			continue
		}
		if c.AppKey != appPublicKeyHex {
			continue
		}
		c.ClosedAt = now
		n++
	}
	return n
}

// CloseStaleGenerations 关闭非当前代际的连接（后台重启/代际切换时调用）。
func (s *ConnectionStore) CloseStale(auths map[string]Auth, gen string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	n := 0
	for id, c := range s.conns {
		if c.ClosedAt != 0 {
			continue
		}
		a, ok := auths[id]
		if !ok || a.Generation == "" || a.Generation != gen {
			c.ClosedAt = now
			n++
		}
	}
	return n
}

// IsOnline 报告连接是否在线（未关闭）。
func (s *ConnectionStore) IsOnline(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[authID]
	return ok && c.ClosedAt == 0
}

// List 返回全部连接记录（含已关闭，供管理面展示）。
func (s *ConnectionStore) List() []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Connection, 0, len(s.conns))
	for _, c := range s.conns {
		out = append(out, *c)
	}
	return out
}

// Online 返回在线连接。
func (s *ConnectionStore) Online() []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Connection
	for _, c := range s.conns {
		if c.ClosedAt == 0 {
			out = append(out, *c)
		}
	}
	return out
}
