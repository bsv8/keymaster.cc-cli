package connect

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// LaunchRecord 为钱包主动启动签发的一次性启动上下文。
// 令牌只在内存有效，不落盘：后台重启后运行代际变化，旧令牌自然失效。
// 令牌绑定签发时钱包独立解析的应用身份（发布者公钥、应用标识、身份摘要与
// 许可范围）、来源、网络、钱包与运行代际，不赋予任意来源或任意身份使用的权限。
type LaunchRecord struct {
	Token             string
	Origin            string
	Network           string
	WalletPublicKey   string
	Generation        string
	AppPublicKey      string
	AppID             string
	AppName           string
	IdentityDigestHex string
	Requirements      []string
	ExpiresAt         int64
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
// verified 为签发时钱包独立解析并经用户确认启动的应用身份；消费时 proof
// 必须与之完全一致，不接受替换身份或扩大权限。
func (s *LaunchStore) Issue(origin, network, walletPub, generation string, verified VerifiedIdentity, ttl time.Duration) (LaunchRecord, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return LaunchRecord{}, err
	}
	now := time.Now()
	rec := LaunchRecord{
		Token:             "launch_" + hex.EncodeToString(b),
		Origin:            origin,
		Network:           network,
		WalletPublicKey:   walletPub,
		Generation:        generation,
		AppPublicKey:      verified.PublisherPublicKey,
		AppID:             verified.AppID,
		AppName:           verified.AppName,
		IdentityDigestHex: verified.IdentityDigestHex,
		Requirements:      append([]string(nil), verified.Requirements...),
		ExpiresAt:         now.Add(ttl).UnixMilli(),
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
