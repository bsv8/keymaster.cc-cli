package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PaymentOutput 为业务输出：金额（聪，允许 0）与 locking script hex。
type PaymentOutput struct {
	Amount uint64 `json:"amount"`
	Script string `json:"script"`
}

// PaymentStatus 与 SDK ConnectPaymentStatus 对齐。
// pending/not_signed 为未提交；unknown 为结果未知；failed 为明确拒绝；
// accepted/already_known 为提交被接受（不代表区块确认）。
type PaymentStatus string

const (
	PaymentPending          PaymentStatus = "pending"
	PaymentNotSigned        PaymentStatus = "not_signed"
	PaymentUnknown          PaymentStatus = "unknown"
	PaymentFailed           PaymentStatus = "failed"
	PaymentAccepted         PaymentStatus = "accepted"
	PaymentAlreadyKnown     PaymentStatus = "already_known"
)

// PaymentRecord 为持久幂等记录；RawHex 为冻结的已签名交易原文（批准前冻结，批准后原样广播）。
// AuthorizationID/IdentityDigest/Requirements/Generation 为创建时的原授权快照，
// 广播前必须复核原授权及运行代际，不能借新授权继续旧申请。
// ApprovalID 为冻结后创建的审批，未创建时为空；重启收敛据此区分“从未审批”与“审批可能已执行”。
type PaymentRecord struct {
	Key               string          `json:"key"`
	PaymentID         string          `json:"paymentId"`
	ClientRequestID   string          `json:"clientRequestId"`
	Origin            string          `json:"origin"`
	OwnerPublicKeyHex string          `json:"ownerPublicKeyHex"`
	Network           string          `json:"network"`
	AppID             string          `json:"appId"`
	AppPublicKey      string          `json:"appPublicKey"`
	AuthorizationID   string          `json:"authorizationId,omitempty"`
	IdentityDigest    string          `json:"identityDigestHex,omitempty"`
	Requirements      []string        `json:"requirements,omitempty"`
	Generation        string          `json:"generation,omitempty"`
	ApprovalID        string          `json:"approvalId,omitempty"`
	RequestDigest     string          `json:"requestDigest"`
	Outputs           []PaymentOutput `json:"outputs"`
	Status            PaymentStatus   `json:"status"`
	Txid              string          `json:"txid,omitempty"`
	FeeSats           uint64          `json:"feeSatoshis,omitempty"`
	ChangeSats        uint64          `json:"changeSatoshis,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	RawHex            string          `json:"rawHex,omitempty"`
	CreatedAt         int64           `json:"createdAt"`
	UpdatedAt         int64           `json:"updatedAt"`
}

// PaymentStore 持久保存付款记录（钱包根/connect/payments.json）。
type PaymentStore struct {
	mu   sync.Mutex
	path string
	byKey map[string]PaymentRecord
}

func NewPaymentStore(walletDir string) *PaymentStore {
	return &PaymentStore{path: filepath.Join(walletDir, "connect", "payments.json"), byKey: map[string]PaymentRecord{}}
}

func (s *PaymentStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var list []PaymentRecord
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	m := map[string]PaymentRecord{}
	for _, r := range list {
		if r.Key == "" {
			continue
		}
		m[r.Key] = r
	}
	s.byKey = m
	return nil
}

func (s *PaymentStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	list := make([]PaymentRecord, 0, len(s.byKey))
	for _, r := range s.byKey {
		list = append(list, r)
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

func (s *PaymentStore) Get(key string) (PaymentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byKey[key]
	return r, ok
}

func (s *PaymentStore) Put(r PaymentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Key == "" {
		return fmt.Errorf("缺少 payment key")
	}
	now := time.Now().UnixMilli()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	prev, hadPrev := s.byKey[r.Key]
	s.byKey[r.Key] = r
	if err := s.saveLocked(); err != nil {
		// 落盘失败回滚内存，保持内存与磁盘一致。
		if hadPrev {
			s.byKey[r.Key] = prev
		} else {
			delete(s.byKey, r.Key)
		}
		return err
	}
	return nil
}

// Delete 删除占位记录（冻结更新落盘失败等 synchronous 失败路径），以便重试可重新占位。
// 持久化失败时返回 error 并回滚内存，调用方按内部错误处理。
func (s *PaymentStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.byKey[key]
	if !ok {
		return nil
	}
	delete(s.byKey, key)
	if err := s.saveLocked(); err != nil {
		s.byKey[key] = prev
		return err
	}
	return nil
}

// DeleteIfUnchanged 条件删除：仅当当前记录与 base 的关键快照一致时才删除，
// 用于清理本流程创建的占位，避免误删后续状态。落盘失败回滚内存并返回 error。
func (s *PaymentStore) DeleteIfUnchanged(key string, base PaymentRecord) (ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, found := s.byKey[key]
	if !found {
		return true, nil
	}
	if cur.PaymentID != base.PaymentID || cur.Status != base.Status || cur.RequestDigest != base.RequestDigest || cur.RawHex != base.RawHex || cur.ApprovalID != base.ApprovalID {
		return false, nil
	}
	delete(s.byKey, key)
	if err := s.saveLocked(); err != nil {
		s.byKey[key] = cur
		return false, err
	}
	return true, nil
}

// PutIfUnchanged 条件更新：仅当当前记录与 base 的关键快照一致时才写入 next，
// 否则返回 ok=false 与当前记录，禁止旧快照覆盖后续状态。
// 比较维度：PaymentID、Status、RequestDigest、RawHex、ApprovalID——任一变化
// 都说明已有更新（审批决定、执行终态、重启收敛）先发生，本次写入必须放弃。
func (s *PaymentStore) PutIfUnchanged(key string, base, next PaymentRecord) (ok bool, cur PaymentRecord, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, found := s.byKey[key]
	if !found {
		return false, PaymentRecord{}, nil
	}
	if cur.PaymentID != base.PaymentID || cur.Status != base.Status || cur.RequestDigest != base.RequestDigest || cur.RawHex != base.RawHex || cur.ApprovalID != base.ApprovalID {
		return false, cur, nil
	}
	if next.Key == "" {
		return false, cur, fmt.Errorf("缺少 payment key")
	}
	now := time.Now().UnixMilli()
	if next.CreatedAt == 0 {
		next.CreatedAt = cur.CreatedAt
	}
	next.UpdatedAt = now
	s.byKey[key] = next
	if err := s.saveLocked(); err != nil {
		// 落盘失败必须恢复旧内存记录，否则内存与磁盘不一致：
		// 调用方看到 ok=false 会误以为是并发冲突而静默跳过收尾，
		// 冻结资金将在当前进程中持续冻结。
		s.byKey[key] = cur
		return false, cur, err
	}
	return true, next, nil
}

// Claim 原子占住请求 ID：不存在时插入占位记录并返回 created=true；
// 已存在时返回现有记录，调用方不得覆盖。占位与限流检查在同一锁内完成，
// 并发相同 ID 不会重复选币、重复冻结或覆盖付款计划。
func (s *PaymentStore) Claim(r PaymentRecord, maxPending int) (created bool, existing PaymentRecord, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Key == "" {
		return false, PaymentRecord{}, fmt.Errorf("缺少 payment key")
	}
	if cur, ok := s.byKey[r.Key]; ok {
		return false, cur, nil
	}
	if maxPending > 0 {
		n := 0
		for _, cur := range s.byKey {
			if cur.Origin == r.Origin && cur.OwnerPublicKeyHex == r.OwnerPublicKeyHex && cur.Network == r.Network && cur.AppID == r.AppID && cur.AppPublicKey == r.AppPublicKey {
				if cur.Status == PaymentPending || cur.Status == PaymentUnknown {
					n++
				}
			}
		}
		if n >= maxPending {
			return false, PaymentRecord{}, fmt.Errorf("payment-pending-limit")
		}
	}
	now := time.Now().UnixMilli()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	s.byKey[r.Key] = r
	if err := s.saveLocked(); err != nil {
		delete(s.byKey, r.Key)
		return false, PaymentRecord{}, err
	}
	return true, r, nil
}

// ReconcileAfterRestart 收敛重启前遗留的未提交审批，避免永久 pending：
//   - pending 且无 RawHex（选币/冻结从未完成）：明确未提交，转 not_signed；
//   - pending、有 RawHex 但无 ApprovalID（冻结完成但审批从未创建）：从未广播，转 not_signed；
//   - pending、有 RawHex 且有 ApprovalID（审批存在过，批准与广播结果已不可考）：转 unknown，保留原申请查询能力。
// unknown/not_signed/failed/accepted/already_known 不动。
func (s *PaymentStore) ReconcileAfterRestart() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	now := time.Now().UnixMilli()
	for key, r := range s.byKey {
		if r.Status != PaymentPending {
			continue
		}
		if r.RawHex == "" {
			r.Status = PaymentNotSigned
			r.Reason = "interrupted"
			r.UpdatedAt = now
			s.byKey[key] = r
			changed = true
			continue
		}
		if r.ApprovalID == "" {
			r.Status = PaymentNotSigned
			r.Reason = "interrupted"
			r.UpdatedAt = now
			s.byKey[key] = r
			changed = true
			continue
		}
		r.Status = PaymentUnknown
		if r.Reason == "" {
			r.Reason = "restart-interrupted"
		}
		r.UpdatedAt = now
		s.byKey[key] = r
		changed = true
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

// CountPending 统计同身份下非终态记录数，用于限流。
func (s *PaymentStore) CountPending(origin, owner, network, appID, publisher string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.byKey {
		if r.Origin == origin && r.OwnerPublicKeyHex == owner && r.Network == network && r.AppID == appID && r.AppPublicKey == publisher {
			if r.Status == PaymentPending || r.Status == PaymentUnknown {
				n++
			}
		}
	}
	return n
}

// PaymentIdentityKey 与钱包侧一致：网络、来源、owner、appId、publisher。
func PaymentIdentityKey(network, origin, owner, appID, publisher string) string {
	raw, _ := json.Marshal([]any{"keymaster.connect-payment-id.v1", network, origin, owner, appID, publisher})
	return string(raw)
}

func PaymentRequestKey(identityKey, clientRequestID string) string {
	raw, _ := json.Marshal([]any{identityKey, clientRequestID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// PaymentRequestDigest 按规范化 outputs 计算：顺序、金额、脚本；同 ID 换内容仍冲突。
func PaymentRequestDigest(identityKey, clientRequestID string, outputs []PaymentOutput, assetID string) string {
	norm := make([]any, 0, len(outputs))
	for _, o := range outputs {
		norm = append(norm, []any{o.Amount, lowerHex(o.Script)})
	}
	raw, _ := json.Marshal([]any{"keymaster.connect-payment-request.v1", identityKey, clientRequestID, norm, assetID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func lowerHex(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'F' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
