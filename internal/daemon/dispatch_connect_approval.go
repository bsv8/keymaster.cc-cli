package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

// sweepExpiredApprovals 收敛过期审批：pending/approved未认领转为 cancelled，
// staged 直接清理；付款类释放确定未提交的冻结；清理附件引用防内存膨胀。
// 调用方在 list/decide/exec 入口调用，后台不依赖页面发送取消。
func (d *Daemon) sweepExpiredApprovals() {
	if d.connectMgr == nil {
		return
	}
	swept := d.connectMgr.Queue().SweepExpired(time.Now().UnixMilli())
	for _, a := range swept {
		if a.Method == "payment.pay" && (a.Status == "cancelled") {
			d.rejectPaymentByApproval(a, "timeout")
		}
		if a.Status == "cancelled" {
			appKey := connect.ApplicationKey(a.Origin, a.AppPublicKey, a.AppID)
			_, _ = d.connectMgr.History().Append(connect.HistoryEvent{
				AppKey: appKey, Origin: a.Origin, AppID: a.AppID, AppPublicKey: a.AppPublicKey,
				Kind: "timed-out", Method: a.Method, Detail: a.ID,
			})
		}
	}
	if len(swept) > 0 {
		d.publishConnect()
	}
}

// publishConnect 发布 Connect 状态变化（待办/连接/许可/历史）。
// 使用统一事件总线 connect 主题；慢消费者按现有背压重同步快照。
func (d *Daemon) publishConnect() {
	if d == nil || d.events == nil || d.connectMgr == nil {
		return
	}
	pending := 0
	for _, a := range d.connectMgr.Queue().List() {
		if a.Status == "pending" {
			pending++
		}
	}
	online := 0
	if d.connectMgr.Connections() != nil {
		online = len(d.connectMgr.Connections().Online())
	}
	d.events.Publish(kmapi.TopicConnect, "", nil, map[string]any{
		"pending": pending, "online": online,
	})
}

// connectSnapshot 返回 connect 主题快照态：待办、在线连接、许可与历史可用性。
func (d *Daemon) connectSnapshot() map[string]any {
	if d.connectMgr == nil {
		return map[string]any{"pending": 0, "online": 0, "historyAvailable": false}
	}
	d.sweepExpiredApprovals()
	pending := 0
	approvals := []map[string]any{}
	for _, a := range d.connectMgr.Queue().List() {
		if a.Status == "pending" {
			pending++
			approvals = append(approvals, a.View())
		}
	}
	online := 0
	if d.connectMgr.Connections() != nil {
		online = len(d.connectMgr.Connections().Online())
	}
	return map[string]any{
		"pending": pending, "online": online,
		"approvals": approvals,
		"historyAvailable": true,
	}
}

// hasAutoPermission 查询对应操作的自动许可。
// 存储不可用时返回 error，调用方必须阻断并报 INTERNAL，不得按无许可继续自动执行。
func (d *Daemon) hasAutoPermission(origin, pubkey, appID, method string) (bool, *kmapi.ErrorDetail) {
	if !connect.IsApprovalMethod(method) {
		return false, nil
	}
	appKey := connect.ApplicationKey(origin, pubkey, appID)
	has, err := d.connectMgr.Permissions().Has(appKey, method)
	if err != nil {
		return false, errDetail(kmapi.ErrInternal, "许可读取失败")
	}
	// 付款读取失败必须明确阻断（Has 已返回 error，上已处理）；此处仅返回布尔。
	return has, nil
}

// grantAutoPermission 仅在批准时保存自动许可；拒绝/取消不保存。
// 写入失败返回 INTERNAL，调用方不得显示“已开启自动同意”。
// 许可与对应历史事件在 AppState 同一锁内一次落盘（原子提交）。
func (d *Daemon) grantAutoPermission(origin, pubkey, appID, method string) *kmapi.ErrorDetail {
	appKey := connect.ApplicationKey(origin, pubkey, appID)
	err := d.connectMgr.AppState().GrantWithEvent(origin, pubkey, appID, method, connect.HistoryEvent{
		AppKey: appKey, Origin: origin, AppID: appID, AppPublicKey: pubkey,
		Kind: "permission-granted", Method: method, Auto: true,
	})
	if err != nil {
		if err.Error() == "already granted" {
			return nil
		}
		return errDetail(kmapi.ErrInternal, "自动许可保存失败")
	}
	return nil
}

// revokeAutoPermission 取消单项自动许可；许可与历史事件在同一锁内一次落盘。
func (d *Daemon) revokeAutoPermission(appKey, method string) *kmapi.ErrorDetail {
	// 先查出归属用于历史（只读快照，不持有锁）。
	var origin, appID, pubkey string
	for _, e := range d.connectMgr.Permissions().Snapshot() {
		if e.AppKey == appKey {
			origin, appID, pubkey = e.Origin, e.AppID, e.AppPublicKey
			break
		}
	}
	if err := d.connectMgr.AppState().RevokeWithEvent(appKey, method, connect.HistoryEvent{
		AppKey: appKey, Origin: origin, AppID: appID, AppPublicKey: pubkey,
		Kind: "permission-revoked", Method: method,
	}); err != nil {
		return errDetail(kmapi.ErrInternal, "取消许可失败")
	}
	return nil
}

// recordApprovalHistory 记录审批决定与执行结果，区分人工/自动与业务失败。
func (d *Daemon) recordApprovalHistory(a connect.Approval, kind string, auto bool, errCode string) {
	appKey := connect.ApplicationKey(a.Origin, a.AppPublicKey, a.AppID)
	_, _ = d.connectMgr.History().Append(connect.HistoryEvent{
		AppKey: appKey, Origin: a.Origin, AppID: a.AppID, AppPublicKey: a.AppPublicKey,
		Kind: kind, Method: a.Method, Auto: auto, ErrorCode: errCode, Detail: a.ID,
	})
}

// validateBusinessParams 校验五类操作请求参数，生成不可被后续请求修改的快照。
// 返回深拷贝后的 params；调用方不得复用传入 map。
func validateBusinessParams(method string, params map[string]any) (map[string]any, *kmapi.ErrorDetail) {
	cp := deepCopyParams(params)
	switch method {
	case "identity.get":
		if err := checkIdentityParams(cp); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	case "intent.sign":
		if err := checkIntentParams(cp); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	case "cipher.encrypt":
		if err := checkCipherEncryptParams(cp); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	case "cipher.decrypt":
		if err := checkCipherDecryptParams(cp); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	case "payment.pay":
		// 付款参数由 parsePaymentPayParams 校验，此处仅检查存在性。
		if _, _, err := parsePaymentPayParams(cp); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	}
	return cp, nil
}

func deepCopyParams(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopyValue(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopyValue(vv)
		}
		return out
	default:
		return v
	}
}

func checkIdentityParams(p map[string]any) error {
	return checkAudIatExpText(p, true)
}

func checkIntentParams(p map[string]any) error {
	if err := checkAudIatExpText(p, true); err != nil {
		return err
	}
	ct, _ := p["contentType"].(string)
	if strings.TrimSpace(ct) == "" {
		return errBad("contentType 必填")
	}
	return nil
}

func checkCipherEncryptParams(p map[string]any) error {
	ct, _ := p["contentType"].(string)
	if strings.TrimSpace(ct) == "" {
		return errBad("contentType 必填")
	}
	return nil
}

func checkCipherDecryptParams(p map[string]any) error {
	return nil
}

func checkAudIatExpText(p map[string]any, needText bool) error {
	aud, _ := p["aud"].(string)
	if strings.TrimSpace(aud) == "" {
		return errBad("aud 必填")
	}
	iat, ok1 := toInt64Any(p["iat"])
	exp, ok2 := toInt64Any(p["exp"])
	if !ok1 || !ok2 {
		return errBad("iat/exp 非法")
	}
	if exp <= iat {
		return errBad("exp 必须大于 iat")
	}
	if needText {
		t, _ := p["text"].(string)
		if strings.TrimSpace(t) == "" {
			return errBad("text 必填")
		}
	}
	return nil
}

func toInt64Any(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	default:
		return 0, false
	}
}

type badErr string

func (e badErr) Error() string { return string(e) }
func errBad(s string) error    { return badErr(s) }

// withAuto 标记自动许可执行的上下文，用于历史区分人工/自动。
type autoKey string

const autoCallerKey autoKey = "km-auto"

func withAuto(ctx context.Context) context.Context {
	return context.WithValue(ctx, autoCallerKey, true)
}

func isAuto(ctx context.Context) bool {
	v, _ := ctx.Value(autoCallerKey).(bool)
	return v
}
// idleConnectionMs 为连接闲置暂停阈值：15 分钟无鉴权活动且无该应用待办时暂停
// 运行连接（保留授权，由 resume 重新握手恢复）。有待办的应用不算离线。
const idleConnectionMs = int64(15 * 60 * 1000)

// watchApprovalExpiry 为后台期限收敛：每 30s 扫描过期审批并终结、淘汰超期终态
// 缓存、收敛闲置/孤儿连接，不依赖页面发送取消通知。
// 关闭时随 lifecycle 退出；扫描本身幂等，重复调用不重复释放冻结。
func (d *Daemon) watchApprovalExpiry(lifecycle context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-lifecycle.Done():
			return
		case <-ticker.C:
			d.sweepExpiredApprovals()
			if d.connectMgr != nil && d.connectMgr.Queue() != nil {
				d.connectMgr.Queue().SweepTerminal(time.Now().UnixMilli())
			}
			d.sweepIdleConnections()
		}
	}
}

// sweepIdleConnections 收敛连接运行态：授权已消失/代际失效的关闭；长时间无活动
// 且无待办的暂停（保留授权）。有待办的应用视为正待用户处理，不暂停。
func (d *Daemon) sweepIdleConnections() {
	if d.connectMgr == nil || d.connectMgr.Connections() == nil {
		return
	}
	now := time.Now().UnixMilli()
	byID := map[string]connect.Auth{}
	for _, a := range d.connectMgr.AuthStore().List() {
		byID[a.AuthorizationID] = a
	}
	gen := d.connectMgr.Generation()
	changed := false
	for _, c := range d.connectMgr.Connections().List() {
		a, ok := byID[c.AuthID]
		if !ok || a.Generation == "" || a.Generation != gen {
			if d.connectMgr.Connections().Close(c.AuthID) {
				changed = true
			}
			continue
		}
	}
	pendingByApp := map[string]bool{}
	for _, ap := range d.connectMgr.Queue().List() {
		if ap.Status == "pending" {
			pendingByApp[connect.ApplicationKey(ap.Origin, ap.AppPublicKey, ap.AppID)] = true
		}
	}
	paused := d.connectMgr.Connections().SweepIdle(now, idleConnectionMs, func(appKey string) bool {
		return pendingByApp[appKey]
	})
	if len(paused) > 0 {
		changed = true
	}
	if changed {
		d.publishConnect()
	}
}
