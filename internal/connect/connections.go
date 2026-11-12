package connect

import (
	"sync"
	"time"
)

// Connection 为应用连接实例（会话）：授权即凭证，连接为运行态。
// ClosedAt != 0 表示已断开（退出/撤销/代际失效）；PausedAt != 0 表示已暂停
// （锁定/刷新/关闭暂停运行连接，保留原授权，由原应用重新握手恢复）。
// 在线定义：未关闭且未暂停的实际运行连接，不能用持久授权条数代替。
type Connection struct {
	ID           string `json:"id"`
	AuthID       string `json:"authId"`
	AppKey       string `json:"appKey"`
	Origin       string `json:"origin"`
	AppID        string `json:"appId"`
	AppPublicKey string `json:"appPublicKey"`
	AppName      string `json:"appName"`
	CreatedAt    int64  `json:"createdAt"`
	LastSeen     int64  `json:"lastSeen"`
	ClosedAt     int64  `json:"closedAt,omitempty"`
	PausedAt     int64  `json:"pausedAt,omitempty"`
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

// UpsertActive 创建或恢复连接为在线（登录/启动/恢复成功时调用）。
func (s *ConnectionStore) UpsertActive(auth Auth) Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if c, ok := s.conns[auth.AuthorizationID]; ok {
		c.AppKey = ApplicationKey(auth.Origin, auth.AppPublicKey, auth.AppID)
		c.Origin, c.AppID, c.AppPublicKey, c.AppName = auth.Origin, auth.AppID, auth.AppPublicKey, auth.AppName
		c.LastSeen = now
		c.ClosedAt = 0
		c.PausedAt = 0
		return *c
	}
	c := &Connection{
		ID: auth.AuthorizationID, AuthID: auth.AuthorizationID,
		AppKey: ApplicationKey(auth.Origin, auth.AppPublicKey, auth.AppID),
		Origin: auth.Origin, AppID: auth.AppID, AppPublicKey: auth.AppPublicKey, AppName: auth.AppName,
		CreatedAt: now, LastSeen: now,
	}
	s.conns[auth.AuthorizationID] = c
	return *c
}

// Touch 更新活跃时间（每次鉴权成功的业务调用）；连接不存在返回 false。
// paused 连接的业务调用应在上层先拒绝，不经 Touch 自动复活。
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

// Pause 暂停运行连接（保留授权）；已关闭/不存在返回 false。
func (s *ConnectionStore) Pause(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[authID]
	if !ok || c.ClosedAt != 0 || c.PausedAt != 0 {
		return false
	}
	c.PausedAt = time.Now().UnixMilli()
	return true
}

// Resume 恢复暂停的连接（resume 握手成功时调用）。
func (s *ConnectionStore) Resume(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[authID]
	if !ok || c.ClosedAt != 0 {
		return false
	}
	c.PausedAt = 0
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

// CloseByApp 关闭该精确应用的全部连接（精确撤销时调用）。
func (s *ConnectionStore) CloseByApp(origin, pubkey, appID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	n := 0
	for _, c := range s.conns {
		if c.ClosedAt != 0 {
			continue
		}
		if c.Origin != origin {
			continue
		}
		if pubkey != "" && !equalFold(c.AppPublicKey, pubkey) {
			continue
		}
		if appID != "" && c.AppID != appID {
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

// SweepIdle 暂停长时间无活动的连接（兜底收敛；保留授权，由 resume 恢复）。
// onlyWhenNoPending 为 true 时，有该应用待办则跳过（用户正待审批不算离线）。
func (s *ConnectionStore) SweepIdle(now int64, idleMs int64, hasPending func(appKey string) bool) []Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Connection
	for _, c := range s.conns {
		if c.ClosedAt != 0 || c.PausedAt != 0 {
			continue
		}
		if now-c.LastSeen < idleMs {
			continue
		}
		if hasPending != nil && hasPending(c.AppKey) {
			continue
		}
		c.PausedAt = now
		out = append(out, *c)
	}
	return out
}

// IsOnline 报告连接是否在线（未关闭且未暂停）。
func (s *ConnectionStore) IsOnline(authID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.conns[authID]
	return ok && c.ClosedAt == 0 && c.PausedAt == 0
}

// List 返回全部连接记录（含已关闭/已暂停，供管理面展示）。
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
		if c.ClosedAt == 0 && c.PausedAt == 0 {
			out = append(out, *c)
		}
	}
	return out
}
