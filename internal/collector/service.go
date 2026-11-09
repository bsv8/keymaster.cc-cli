package collector

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// EarlyTriggerCount 超过 1000 条（即 1001 条）提前触发。HourlyInterval 每小时触发。
const (
	EarlyTriggerCount = 1001
	HourlyInterval    = time.Hour
)

// 生产缺省退避：基线 30s、因子 2、上限 1h。
const (
	retryBase = 30 * time.Second
	retryCap  = time.Hour
)

var secpN, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141", 16)
var halfOrder = new(big.Int).Rsh(secpN, 1)

// SignFunc 对 32 字节摘要签名，返回 low-S DER 与压缩公钥 hex。
type SignFunc func(digest []byte) (der []byte, pubkeyHex string, err error)

// HostPrivateKeySigner 用钱包私钥构造签名函数：只签给定摘要，私钥不外泄。
func HostPrivateKeySigner(privateKey []byte) (SignFunc, error) {
	key, _ := ec.PrivateKeyFromBytes(privateKey)
	if key == nil {
		return nil, fmt.Errorf("collector: 私钥无效")
	}
	pubkeyHex := hex.EncodeToString(key.PubKey().Compressed())
	return func(digest []byte) ([]byte, string, error) {
		if len(digest) != 32 {
			return nil, "", fmt.Errorf("collector: 摘要必须为 32 字节")
		}
		sig, err := key.Sign(digest)
		if err != nil {
			return nil, "", err
		}
		if sig.S.Cmp(halfOrder) == 1 {
			sig.S.Sub(secpN, sig.S)
		}
		der, err := sig.ToDER()
		if err != nil {
			return nil, "", err
		}
		return der, pubkeyHex, nil
	}, nil
}

// VerifyDigestSignature 校验摘要签名：严格 DER、low-S、ECDSA 三门。
func VerifyDigestSignature(pubkeyHex string, digest [32]byte, signature []byte) error {
	pubRaw, err := hex.DecodeString(pubkeyHex)
	if err != nil || len(pubRaw) != 33 {
		return fmt.Errorf("collector: 公钥非法")
	}
	pub, err := ec.ParsePubKey(pubRaw)
	if err != nil {
		return fmt.Errorf("collector: 公钥非法: %w", err)
	}
	if !bytes.Equal(pub.Compressed(), pubRaw) {
		return fmt.Errorf("collector: 公钥非规范压缩编码")
	}
	sig, err := ec.ParseDERSignature(signature)
	if err != nil {
		return fmt.Errorf("collector: 签名 DER 非法: %w", err)
	}
	roundTrip, err := sig.ToDER()
	if err != nil || !bytes.Equal(roundTrip, signature) {
		return fmt.Errorf("collector: 签名非规范 DER")
	}
	if sig.S.Cmp(halfOrder) == 1 {
		return fmt.Errorf("collector: 拒绝 high-S 签名")
	}
	if !sig.Verify(digest[:], pub) {
		return fmt.Errorf("collector: 验签失败")
	}
	return nil
}

// Options 是采集服务的装配参数。
type Options struct {
	// Endpoint 采集服务地址。Enabled 启用开关（与配置同源）。
	Endpoint string
	Enabled  bool
	// Sign 钱包签名函数。Client 提交 HTTP 客户端。
	Sign   SignFunc
	Client *http.Client
	// Now 注入时钟，便于测试。RetryBase 注入退避基线，
	// RetryCap 注入退避上限；EarlyTriggerCount 与 SubmitInterval
	// 注入任务间隔参数（零值用生产缺省）。
	Now               func() time.Time
	RetryBase         time.Duration
	RetryCap          time.Duration
	EarlyTriggerCount int
	SubmitInterval    time.Duration
}

// Service 是 CLI 采集服务：同一时刻只发送一个批次，失败复用原字节重试。
type Service struct {
	opts     Options
	store    *Store
	mu       sync.Mutex
	sending  bool
	retryIn  time.Duration
	retryAt  time.Time
	lastTick time.Time
}

// New 打开状态并构造服务。
func New(statePath string, opts Options) (*Service, error) {
	store, err := Open(statePath)
	if err != nil {
		return nil, err
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RetryBase <= 0 {
		opts.RetryBase = retryBase
	}
	return &Service{opts: opts, store: store, retryIn: opts.RetryBase}, nil
}

// sameRecord 比较提交内容（名称、介绍、长度、MIME 全比）。
func sameRecord(a, b Record) bool {
	return a.FileName == b.FileName && a.Description == b.Description &&
		a.FileSizeBytes == b.FileSizeBytes && a.MediaType == b.MediaType
}

// NotifyStored 合并单条记录；与已确认一致则 noop。介绍取自采集自有存储。
func (s *Service) NotifyStored(rec Record) {
	_ = s.store.Update(func(doc *State) error {
		rec.Description = doc.Descriptions[rec.SeedHashHex]
		if known, ok := doc.Confirmed[rec.SeedHashHex]; ok &&
			known.FileName == rec.FileName && known.Description == rec.Description {
			if queued, has := doc.Pending[rec.SeedHashHex]; !has || sameRecord(queued, rec) {
				return nil
			}
		}
		doc.Pending[rec.SeedHashHex] = rec
		return nil
	})
}

// SetDescription 保存介绍并重新入队；无记录可附着时只保存介绍。
func (s *Service) SetDescription(seedHashHex, description string) error {
	if len(description) > 2048 {
		return fmt.Errorf("collector: 介绍超出 2048 字节")
	}
	return s.store.Update(func(doc *State) error {
		doc.Descriptions[seedHashHex] = description
		if queued, ok := doc.Pending[seedHashHex]; ok {
			if queued.Description == description {
				if known, has := doc.Confirmed[seedHashHex]; has && known.Description == description {
					return nil
				}
			}
			queued.Description = description
			doc.Pending[seedHashHex] = queued
			return nil
		}
		if known, ok := doc.Confirmed[seedHashHex]; ok {
			if known.Description == description {
				return nil
			}
			known.Description = description
			doc.Pending[seedHashHex] = known
		}
		return nil
	})
}

// SetTransport 更新地址与开关。
func (s *Service) SetTransport(endpoint string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Endpoint = endpoint
	s.opts.Enabled = enabled
}

// Status 返回展示用状态：待提交数、确认数、操作历史、最近成功/失败。
func (s *Service) Status() map[string]any {
	doc := s.store.Snapshot()
	history := doc.History
	if history == nil {
		history = []Submission{}
	}
	if len(history) > 20 {
		history = history[len(history)-20:]
	}
	out := map[string]any{
		"enabled":        s.opts.Enabled,
		"endpoint":       s.opts.Endpoint,
		"pendingCount":   len(doc.Pending),
		"confirmedCount": len(doc.Confirmed),
		"inflight":       doc.Inflight != nil,
		"lastError":      doc.Meta.LastError,
		"history":        history,
	}
	if doc.Meta.LastSuccessAt != nil {
		out["lastSuccessAt"] = *doc.Meta.LastSuccessAt
	} else {
		out["lastSuccessAt"] = nil
	}
	return out
}

// earlyTriggerCount 返回生效的提前触发阈值。
func (s *Service) earlyTriggerCount() int {
	if s.opts.EarlyTriggerCount > 0 {
		return s.opts.EarlyTriggerCount
	}
	return EarlyTriggerCount
}

// submitInterval 返回生效的周期提交间隔。
func (s *Service) submitInterval() time.Duration {
	if s.opts.SubmitInterval > 0 {
		return s.opts.SubmitInterval
	}
	return HourlyInterval
}

// retryCap 返回生效的退避上限。
func (s *Service) retryCap() time.Duration {
	if s.opts.RetryCap > 0 {
		return s.opts.RetryCap
	}
	return retryCap
}

// shouldTrigger 判定 early/hourly。
func (s *Service) shouldTrigger(doc *State, now time.Time) string {
	if len(doc.Pending) >= s.earlyTriggerCount() {
		return "early"
	}
	if now.Sub(time.UnixMilli(doc.Meta.LastAttemptAt)) >= s.submitInterval() {
		return "hourly"
	}
	return ""
}

// Tick 供外部调度（目录核对后、定时器）调用：有触发条件即提交。
func (s *Service) Tick(ctx context.Context) {
	if !s.opts.Enabled || s.opts.Endpoint == "" {
		return
	}
	doc := s.store.Snapshot()
	if doc.Inflight != nil {
		return
	}
	if s.shouldTrigger(&doc, s.opts.Now()) == "" {
		return
	}
	s.submit(ctx, "tick")
}

// Flush 忽略计时器立即尝试（管理入口用）。
func (s *Service) Flush(ctx context.Context) bool {
	if !s.opts.Enabled || s.opts.Endpoint == "" {
		return false
	}
	if len(s.store.Snapshot().Pending) == 0 {
		return false
	}
	s.submit(ctx, "manual")
	return true
}

// RetryNow 复用原字节立即重试（管理入口用）。
func (s *Service) RetryNow(ctx context.Context) bool {
	if s.store.Snapshot().Inflight == nil {
		return false
	}
	s.sendInflight(ctx)
	return true
}

// Run 启动调度循环：每小时检查 + 退避重试；ctx 取消即停。
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// 启动即恢复：未完成批次直接重发同一字节。
	if s.store.Snapshot().Inflight != nil && s.opts.Enabled && s.opts.Endpoint != "" {
		s.sendInflight(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_ = now
			if !s.opts.Enabled || s.opts.Endpoint == "" {
				continue
			}
			doc := s.store.Snapshot()
			if doc.Inflight != nil {
				if !s.opts.Now().Before(s.retryAt) {
					s.sendInflight(ctx)
				}
				continue
			}
			if s.shouldTrigger(&doc, s.opts.Now()) != "" {
				s.submit(ctx, "hourly")
			}
		}
	}
}

func (s *Service) submit(ctx context.Context, trigger string) {
	s.mu.Lock()
	if s.sending {
		s.mu.Unlock()
		return
	}
	s.sending = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sending = false
		s.mu.Unlock()
	}()
	var records []Record
	_ = s.store.Update(func(doc *State) error {
		for _, r := range doc.Pending {
			records = append(records, r)
		}
		return nil
	})
	if len(records) == 0 {
		return
	}
	canonical, err := CanonicalJSON(records)
	if err != nil {
		s.fail(err.Error())
		return
	}
	gzipBytes, err := GzipJSON(canonical)
	if err != nil {
		s.fail(err.Error())
		return
	}
	digest := DigestGzip(gzipBytes)
	if s.opts.Sign == nil {
		s.fail("collector: 签名能力缺失")
		return
	}
	der, pubkeyHex, err := s.opts.Sign(digest[:])
	if err != nil {
		s.fail(err.Error())
		return
	}
	if err := VerifyDigestSignature(pubkeyHex, digest, der); err != nil {
		s.fail(err.Error())
		return
	}
	now := s.opts.Now().UnixMilli()
	_ = s.store.Update(func(doc *State) error {
		byHash := make(map[string]Record, len(records))
		for _, r := range records {
			byHash[r.SeedHashHex] = r
		}
		doc.Inflight = &Inflight{Gzip: gzipBytes, Signature: der, PubkeyHex: pubkeyHex, Records: byHash, Trigger: trigger}
		doc.Meta.LastAttemptAt = now
		return nil
	})
	s.sendInflight(ctx)
}

func (s *Service) sendInflight(ctx context.Context) {
	doc := s.store.Snapshot()
	inflight := doc.Inflight
	if inflight == nil || s.opts.Endpoint == "" {
		return
	}
	// 提交地址为完整 URL（含 path 原样使用，旧基地址自动补路径）。
	url := SubmissionURL(s.opts.Endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(inflight.Gzip))
	if err != nil {
		s.fail(err.Error())
		return
	}
	req.Header.Set("Content-Type", ContentType)
	req.Header.Set(HeaderPubkey, inflight.PubkeyHex)
	req.Header.Set(HeaderSignature, hex.EncodeToString(inflight.Signature))
	req.Header.Set(HeaderSigVer, "1")
	res, err := s.opts.Client.Do(req)
	if err != nil {
		s.fail(err.Error())
		return
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		// 401/400 不可重试：丢弃该批次，保留队列待下次变化重建。
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusBadRequest {
			_ = s.store.Update(func(doc *State) error {
				doc.Inflight = nil
				doc.Meta.LastError = fmt.Sprintf("rejected: http-%d", res.StatusCode)
				return nil
			})
			return
		}
		s.fail(fmt.Sprintf("http-%d", res.StatusCode))
		return
	}
	var receipt struct {
		ArchiveID string `json:"archiveId"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil || receipt.ArchiveID == "" {
		s.fail("collector: 回执缺 archiveId")
		return
	}
	now := s.opts.Now().UnixMilli()
	_ = s.store.Update(func(doc *State) error {
		trigger, count, archive := "", 0, ""
		if doc.Inflight != nil {
			trigger = doc.Inflight.Trigger
			count = len(doc.Inflight.Records)
		}
		if doc.Inflight == nil {
			return nil
		}
		for hash, sent := range doc.Inflight.Records {
			if current, ok := doc.Pending[hash]; ok {
				if sameRecord(current, sent) {
					doc.Confirmed[hash] = sent
					delete(doc.Pending, hash)
				}
			} else {
				doc.Confirmed[hash] = sent
			}
		}
		archive = receipt.ArchiveID
		doc.Inflight = nil
		doc.Meta.LastSuccessAt = &now
		doc.Meta.LastError = ""
		doc.History = append(doc.History, Submission{ArchiveID: archive, At: now, Trigger: trigger, Count: count})
		if len(doc.History) > 50 {
			doc.History = doc.History[len(doc.History)-50:]
		}
		_ = archive
		return nil
	})
	s.mu.Lock()
	s.retryIn = s.opts.RetryBase
	s.mu.Unlock()
}

func (s *Service) fail(reason string) {
	_ = s.store.Update(func(doc *State) error {
		doc.Meta.LastError = reason
		return nil
	})
	s.mu.Lock()
	s.retryAt = s.opts.Now().Add(s.retryIn)
	s.retryIn *= 2
	if s.retryIn > s.retryCap() {
		s.retryIn = s.retryCap()
	}
	s.mu.Unlock()
}
