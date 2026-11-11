package connect

import (
	"fmt"
	"sync"
	"time"
)

// Approval 为待办审批（不跨重启恢复）。原请求按批准时的快照保存，决定只
// 引用审批 ID；批准后由执行路径认领一次并执行，结果随认领返回，重复认领
// 返回已保存的结果，不重复执行。
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
	// ExecState 为空表示尚未认领；running 表示正在执行；done/failed 是
	// 已保存的终态。并发轮询必须能区分“正在执行”和“已有终态”。
	ExecState     string            `json:"-"`
	Result        any               `json:"-"`
	Binaries      map[string][]byte `json:"-"`
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

func (q *Queue) Create(appPubkey, appID, appName, method string, params map[string]any, origin string, requirements []string, digest string) *Approval {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	id := fmt.Sprintf("ap_%d_%d", time.Now().UnixMilli(), q.seq)
	a := &Approval{
		ID: id, AppPublicKey: appPubkey, AppID: appID, AppName: appName, Method: method,
		Params: params, Origin: origin,
		Requirements:   append([]string(nil), requirements...),
		IdentityDigest: digest,
		Status:         "pending", CreatedAt: time.Now().UnixMilli(),
	}
	q.items[id] = a
	cp := *a
	return &cp
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
func (q *Queue) Decide(id, decision string) (Approval, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	a, ok := q.items[id]
	if !ok {
		return Approval{}, false, fmt.Errorf("审批不存在")
	}
	if a.Status != "pending" {
		return *a, false, nil
	}
	switch decision {
	case "approve":
		a.Status = "approved"
	case "reject":
		a.Status = "rejected"
	case "cancel":
		a.Status = "cancelled"
	default:
		return Approval{}, false, fmt.Errorf("决定非法")
	}
	a.DecidedAt = time.Now().UnixMilli()
	return *a, true, nil
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
