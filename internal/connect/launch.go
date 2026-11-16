package connect

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// LaunchRecord 为钱包主动启动签发的一次性启动上下文。
// 令牌只在内存有效，不落盘：后台重启后运行代际变化，旧令牌自然失效。
// 令牌绑定已确认的目标 URL 推导的 origin、原钱包、来源、启动窗口与运行代际，
// 保留期限、单次消费及防重放；跨 origin 不能消费。
type LaunchRecord struct {
	URL                     string
	DeploymentGeneration    string
	ExpectedAppPublicKeyHex string
	DeploymentID            string
	Token                   string
	Origin                  string
	Network                 string
	WalletPublicKey         string
	Generation              string
	ExpiresAt               int64
}

// LaunchStore 持有未消费的一次性启动令牌。
type LaunchStore struct {
	mu      sync.Mutex
	records map[string]LaunchRecord
}

func NewLaunchStore() *LaunchStore {
	return &LaunchStore{records: map[string]LaunchRecord{}}
}

// LaunchTTL 为启动令牌有效期：钱包点击启动后 Demo 应立即自动 launch。
const LaunchTTL = 5 * time.Minute

// Issue 签发一次性令牌并顺带清理已过期记录。
func (s *LaunchStore) Issue(origin, network, walletPub, generation string, ttl time.Duration, target ...string) (LaunchRecord, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return LaunchRecord{}, err
	}
	now := time.Now()
	canonical, err := CanonicalOrigin(origin)
	if err != nil {
		return LaunchRecord{}, err
	}
	rec := LaunchRecord{
		Token:           "launch_" + hex.EncodeToString(b),
		Origin:          canonical,
		Network:         network,
		WalletPublicKey: walletPub,
		Generation:      generation,
		ExpiresAt:       now.Add(ttl).UnixMilli(),
	}
	if len(target) > 0 {
		rec.ExpectedAppPublicKeyHex = target[0]
	}
	if len(target) > 1 {
		rec.DeploymentID = target[1]
	}
	if len(target) > 2 {
		rec.URL = target[2]
	}
	if len(target) > 3 {
		rec.DeploymentGeneration = target[3]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, old := range s.records {
		if old.ExpiresAt <= now.UnixMilli() {
			delete(s.records, token)
		}
	}
	s.records[rec.Token] = rec
	return rec, nil
}

// Consume 取出并删除令牌；调用方负责校验来源、过期与运行上下文。
// 先删除再校验：重放、错误来源与过期一律只生效一次。
func (s *LaunchStore) Consume(token string) (LaunchRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[token]
	if ok {
		delete(s.records, token)
	}
	return rec, ok
}

// InvalidateDeployment 只撤销该监听代际的令牌，不影响端口复用后的新实例。
func (s *LaunchStore) InvalidateDeployment(origin, deploymentID, generation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, record := range s.records {
		if record.Origin == origin && record.DeploymentID == deploymentID && record.DeploymentGeneration == generation {
			delete(s.records, token)
		}
	}
}
