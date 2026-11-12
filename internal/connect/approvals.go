package connect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Approval 为待办审批（不跨重启恢复）。原请求按批准时的快照保存，决定只
// 引用审批 ID；批准后由执行路径认领一次并执行，结果随认领返回，重复认领
// 返回已保存的结果，不重复执行。
//
// 付款审批必须先持久化绑定再可见：创建时为 staged（不进入待办列表、不能被
// 决定或认领），落盘成功后 Publish 转为 pending。避免“先可见后落盘”窗口内
// 的决定/执行与迟到回写互相覆盖。
type Approval struct {
	ID             string         `json:"id"`
	AppPublicKey   string         `json:"appPublicKey"`
	AppID          string         `json:"appId"`
	AppName        string         `json:"appName"`
	Method         string         `json:"method"`
	Params         map[string]any `json:"params"`
	Origin         string         `json:"origin"`
	Requirements   []string       `json:"requirements"`
	IdentityDigest string         `json:"identityDigestHex"`
	Status         string         `json:"status"`
	CreatedAt      int64          `json:"createdAt"`
	DecidedAt      int64          `json:"decidedAt,omitempty"`
	// TerminalAt 为进入终态（rejected/cancelled/done/failed）的时间，用于终态缓存淘汰；
	// 期限内重复查询返回同一结果，超期后删除释放附件内存。
	TerminalAt     int64          `json:"-"`
	// AuthorizationID 为创建时的原连接授权引用；登录类审批为空。
	AuthorizationID string `json:"authorizationId,omitempty"`
	// TransportRequestID 为原传输请求 ID（Receiver 请求 ID），身份/意图信封
	// 以此为 requestId，保证与 cc 一致的可关联性；缺失时回退审批 ID。
	TransportRequestID string `json:"-"`
	// ExpiresAt 为请求期限（epoch ms）；超时后不可批准，后台期限收敛终结。
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	// ExecState 为空表示尚未认领；running 表示正在执行；done/failed 是
	// 已保存的终态。并发轮询必须能区分“正在执行”和“已有终态”。
	ExecState     string            `json:"-"`
	Result        any               `json:"-"`
	Binaries      map[string][]byte `json:"-"`
	ReqBinaries   map[string][]byte `json:"-"`
	ExecErrorCode string            `json:"-"`
	ExecErrorMsg  string            `json:"-"`
}

// Queue 管理审批及竞争：批准/拒绝/取消只能有一个转换成功，重复决定不重复执行。
type Queue struct {
	mu    sync.Mutex
	items map[string]*Approval
	seq   int64
}

func NewQueue() *Queue {
	return &Queue{items: map[string]*Approval{}}
}

// DefaultApprovalTTL 为待办默认期限：10 分钟。超时后不可批准，由 SweepExpired 收敛。
const DefaultApprovalTTL = int64(10 * 60 * 1000)

func approvalExpiry(params map[string]any, now int64) int64 {
    // 请求自带 exp（unix seconds）时取更早者作为期限。
    if v, ok := params["exp"]; ok {
        var expSec int64
        switch n := v.(type) {
        case float64:
            expSec = int64(n)
        case int:
            expSec = int64(n)
        case int64:
            expSec = n
        }
        if expSec > 0 {
            expMs := expSec * 1000
            if expMs < now+DefaultApprovalTTL {
                return expMs
            }
        }
    }
    return now + DefaultApprovalTTL
}

func (q *Queue) Create(appPubkey, appID, appName, method string, params map[string]any, origin string, requirements []string, digest string) *Approval {
	return q.CreateFull("", "", appPubkey, appID, appName, method, params, origin, requirements, digest, nil)
}

// CreateFull 一次装配完整待办：原授权引用、原传输请求 ID、期限、参数快照与二进制
// 附件在同一锁内写入并深拷贝，返回前即进入可批准队列；不存在“先可见后补齐”窗口。
func (q *Queue) CreateFull(authID, transportReqID, appPubkey, appID, appName, method string, params map[string]any, origin string, requirements []string, digest string, reqBinaries map[string][]byte) *Approval {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	id := fmt.Sprintf("ap_%d_%d", time.Now().UnixMilli(), q.seq)
	now := time.Now().UnixMilli()
	cpParams := deepCopyApprovalParams(params)
	a := &Approval{
		ID: id, AppPublicKey: appPubkey, AppID: appID, AppName: appName, Method: method,
		Params: cpParams, Origin: origin,
		Requirements:   append([]string(nil), requirements...),
		IdentityDigest: digest,
		Status:         "pending", CreatedAt: now,
		AuthorizationID: authID, TransportRequestID: transportReqID,
		ExpiresAt: approvalExpiry(cpParams, now), ReqBinaries: copyApprovalBinaries(reqBinaries),
	}
	q.items[id] = a
	cp := *a
	return &cp
}

// CreateStagedFull 一次装配完整暂存审批：落盘绑定完成前不进入待办列表，不能被
// 决定或认领；调用方必须在持久化成功后 Publish，未成功则 Discard。
func (q *Queue) CreateStagedFull(authID, transportReqID, appPubkey, appID, appName, method string, params map[string]any, origin string, requirements []string, digest string, reqBinaries map[string][]byte) *Approval {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	id := fmt.Sprintf("ap_%d_%d", time.Now().UnixMilli(), q.seq)
	now := time.Now().UnixMilli()
	cpParams := deepCopyApprovalParams(params)
	a := &Approval{
		ID: id, AppPublicKey: appPubkey, AppID: appID, AppName: appName, Method: method,
		Params: cpParams, Origin: origin,
		Requirements:   append([]string(nil), requirements...),
		IdentityDigest: digest,
		Status:         "staged", CreatedAt: now,
		AuthorizationID: authID, TransportRequestID: transportReqID,
		ExpiresAt: approvalExpiry(cpParams, now), ReqBinaries: copyApprovalBinaries(reqBinaries),
	}
	q.items[id] = a
	cp := *a
	return &cp
}

func deepCopyApprovalParams(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyApprovalValue(v)
	}
	return out
}

func deepCopyApprovalValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyApprovalValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyApprovalValue(vv)
		}
		return out
	default:
		return v
	}
}

func copyApprovalBinaries(m map[string][]byte) map[string][]byte {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// Publish 将暂存审批转为可见待办。只有 staged 能发布，防止重复发布或发布已终态审批。
func (q *Queue) Publish(id string) (Approval, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok || a.Status != "staged" {
		return Approval{}, false
	}
	a.Status = "pending"
	return *a, true
}

// Discard 丢弃暂存审批（持久化失败时清理，避免留下不可见但占位的审批）。
// 已发布或已终态的审批不能丢弃，只能走正常决定流程。
func (q *Queue) Discard(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok || a.Status != "staged" {
		return false
	}
	delete(q.items, id)
	return true
}

func (q *Queue) List() []Approval {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Approval, 0, len(q.items))
	for _, a := range q.items {
		if a.Status == "pending" {
			out = append(out, *a)
		}
	}
	return out
}

func (q *Queue) Get(id string) (Approval, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok {
		return Approval{}, false
	}
	return *a, true
}

// Decide 尝试终态转换；仅 pending 可转换，重复决定返回当前状态不重复执行。
// staged 审批尚未发布，不接受决定，避免对未完成持久绑定的审批提前决策。
func (q *Queue) Decide(id, decision string) (Approval, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok {
		return Approval{}, false, fmt.Errorf("审批不存在")
	}
	if a.Status == "staged" {
		return Approval{}, false, fmt.Errorf("审批尚未发布")
	}
	if a.Status != "pending" {
		return *a, false, nil
	}
	if a.IsExpired(time.Now().UnixMilli()) {
		now := time.Now().UnixMilli()
		a.Status = "cancelled"
		a.DecidedAt = now
		a.TerminalAt = now
		a.Binaries = nil
		a.ReqBinaries = nil
		return *a, false, fmt.Errorf("审批已过期")
	}
	now := time.Now().UnixMilli()
	switch decision {
	case "approve":
		a.Status = "approved"
	case "reject":
		a.Status = "rejected"
		a.TerminalAt = now
		a.ReqBinaries = nil
	case "cancel":
		a.Status = "cancelled"
		a.TerminalAt = now
		a.ReqBinaries = nil
	default:
		return Approval{}, false, fmt.Errorf("决定非法")
	}
	a.DecidedAt = now
	return *a, true, nil
}

// DecideApproveWithGrant 原子批准并提交许可：pending→approved 与 grant 回调在同一
// 把锁内完成，期间认领（ClaimExecution）、终结（AbandonApproved）与其他决定全部
// 阻塞等待；grant 失败则回退为 pending，不留下可执行的批准状态。
// grant 回调不得再调用 Queue 方法（否则死锁），只做 AppState 等外部提交。
// 返回 first==true 表示本次批准生效；deduped 时返回现状不重复执行 grant。
func (q *Queue) DecideApproveWithGrant(id string, grant func(Approval) error) (Approval, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok {
		return Approval{}, false, fmt.Errorf("审批不存在")
	}
	if a.Status == "staged" {
		return Approval{}, false, fmt.Errorf("审批尚未发布")
	}
	if a.Status != "pending" {
		return *a, false, nil
	}
	now := time.Now().UnixMilli()
	if a.IsExpired(now) {
		a.Status = "cancelled"
		a.DecidedAt = now
		a.TerminalAt = now
		a.Binaries = nil
		a.ReqBinaries = nil
		return *a, false, fmt.Errorf("审批已过期")
	}
	a.Status = "approved"
	a.DecidedAt = now
	snapshot := *a
	if err := grant(snapshot); err != nil {
		a.Status = "pending"
		a.DecidedAt = 0
		return *a, false, err
	}
	return *a, true, nil
}

// AbandonApproved 原子终结已批准但尚未认领的审批：仅当 status==approved
// 且 ExecState 为空（确定未提交认领是执行的唯一入口未认领即未提交）时转为
// cancelled；已认领或已终态的不动返回 false。调用方负责释放关联冻结。
// 与 ClaimExecution 共用同一把锁两者有且仅有一者成功。
func (q *Queue) AbandonApproved(id string) (Approval, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok || a.Status != "approved" || a.ExecState != "" {
		return Approval{}, false
	}
	a.Status = "cancelled"
	now := time.Now().UnixMilli()
	a.DecidedAt = now
	a.TerminalAt = now
	a.ReqBinaries = nil
	return *a, true
}

// ClaimExecution 认领一次已批准审批的执行权；仅第一次返回 true，调用方负责
// 执行并随后调用 StoreResult/StoreError。并发认领中仅一者成功。
func (q *Queue) ClaimExecution(id string) (Approval, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok || a.Status != "approved" || a.ExecState != "" {
		return Approval{}, false
	}
	if a.IsExpired(time.Now().UnixMilli()) {
		return Approval{}, false
	}
	a.ExecState = "running"
	return *a, true
}

// StoreResult 保存成功的终态结果（含二进制），供重复查询返回同一结果。
func (q *Queue) StoreResult(id string, result any, binaries map[string][]byte) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if a, ok := q.items[id]; ok {
		a.ExecState = "done"
		a.Result = result
		a.Binaries = binaries
		a.TerminalAt = time.Now().UnixMilli()
		a.ReqBinaries = nil
	}
}

// StoreError 保存失败的终态；重复查询返回同一错误，不能伪装成空成功。
func (q *Queue) StoreError(id, code, message string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if a, ok := q.items[id]; ok {
		a.ExecState = "failed"
		a.ExecErrorCode = code
		a.ExecErrorMsg = message
		a.TerminalAt = time.Now().UnixMilli()
		a.ReqBinaries = nil
	}
}

// CompletedResult 只在已有明确终态时返回结果；running 必须继续等待或报告冲突。
// 返回值 ok=false 表示尚在执行或尚未认领。
func (q *Queue) CompletedResult(id string) (result any, binaries map[string][]byte, code, message string, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, found := q.items[id]
	if !found || (a.ExecState != "done" && a.ExecState != "failed") {
		return nil, nil, "", "", false
	}
	return a.Result, a.Binaries, a.ExecErrorCode, a.ExecErrorMsg, true
}
// IsExpired 判断审批是否已过期限。
func (a Approval) IsExpired(now int64) bool {
	if a.ExpiresAt == 0 {
		return false
	}
	return now >= a.ExpiresAt
}

// SweepExpired 收敛过期审批：pending 过期转为 cancelled；approved 未认领过期
// 转为 cancelled（调用方负责释放冻结）。已认领/已终态不动。返回被收敛的审批。
// 同时清理过期 staged 占位，避免无限占用内存；清理时丢弃二进制附件引用。
func (q *Queue) SweepExpired(now int64) []Approval {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []Approval
	for id, a := range q.items {
		if !a.IsExpired(now) {
			continue
		}
		switch a.Status {
		case "pending":
			a.Status = "cancelled"
			a.DecidedAt = now
			a.TerminalAt = now
			a.Binaries = nil
			a.ReqBinaries = nil
			out = append(out, *a)
		case "approved":
			if a.ExecState == "" {
				a.Status = "cancelled"
				a.DecidedAt = now
				a.TerminalAt = now
				a.Binaries = nil
				a.ReqBinaries = nil
				out = append(out, *a)
			}
		case "staged":
			delete(q.items, id)
			cp := *a
			cp.Binaries = nil
			out = append(out, cp)
		}
	}
	return out
}

// View 返回用于 UI 的筛选展示，不返回二进制附件与敏感明文。
// 加解密附附件大小摘要，意图附内容大小与 SHA-256，付款由调用方按冻结快照展示。
func (a Approval) View() map[string]any {
	params := redactParamsForView(a.Method, a.Params)
	view := map[string]any{
		"id": a.ID, "appPublicKey": a.AppPublicKey, "appId": a.AppID, "appName": a.AppName,
		"method": a.Method, "params": params, "origin": a.Origin,
		"requirements": append([]string(nil), a.Requirements...),
		"identityDigestHex": a.IdentityDigest, "status": a.Status, "createdAt": a.CreatedAt,
		"expiresAt": a.ExpiresAt, "authorizationId": a.AuthorizationID,
	}
	if len(a.ReqBinaries) > 0 {
		sizes := make(map[string]any, len(a.ReqBinaries))
		for k, v := range a.ReqBinaries {
			sizes[k] = len(v)
		}
		view["attachmentSizes"] = sizes
		if a.Method == "intent.sign" {
			if content, ok := a.ReqBinaries["content"]; ok {
				sum := sha256.Sum256(content)
				view["contentSize"] = len(content)
				view["contentSha256Hex"] = hex.EncodeToString(sum[:])
			}
		}
	} else if a.Method == "intent.sign" {
		view["contentSize"] = 0
	}
	return view
}

func redactParamsForView(method string, params map[string]any) map[string]any {
	if params == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = v
	}
	if method == "cipher.decrypt" {
		if c, ok := out["content"]; ok {
			if str, ok := c.(string); ok && len(str) > 64 {
				out["content"] = map[string]any{"redacted": true, "size": len(str)}
			}
		}
	}
	return out
}

// TerminalRetentionMs 为终态结果缓存保留期限：30 分钟。期限内重复查询返回同一
// 结果（含二进制），超期后删除整条记录释放附件内存。运行中的认领不受影响。
const TerminalRetentionMs = int64(30 * 60 * 1000)

// SweepTerminal 淘汰超期终态审批，返回被删除的条数。
// 仅删除 rejected/cancelled/done/failed 且 TerminalAt 超期的记录；pending、
// staged、approved 未认领/执行中一律保留。调用方负责释放关联的付款冻结
// （cancelled 的付款由上层 sweepExpired 处理，终态淘汰时冻结早已释放）。
func (q *Queue) SweepTerminal(now int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for id, a := range q.items {
		if a.TerminalAt == 0 {
			continue
		}
		terminal := a.Status == "rejected" || a.Status == "cancelled" ||
			a.ExecState == "done" || a.ExecState == "failed"
		if !terminal {
			continue
		}
		if now-a.TerminalAt < TerminalRetentionMs {
			continue
		}
		delete(q.items, id)
		n++
	}
	return n
}

// EnvelopeRequestID 返回信封用 requestId：优先原传输请求 ID，缺失回退审批 ID。
func (a Approval) EnvelopeRequestID() string {
	if a.TransportRequestID != "" {
		return a.TransportRequestID
	}
	return a.ID
}
