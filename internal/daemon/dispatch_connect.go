package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

// 建立连接类方法：每次调用都需用户确认绑定，不做静默自动执行。
func isEstablishingConnectMethod(method string) bool {
	switch method {
	case "connect.login", "connect.launch":
		return true
	default:
		return false
	}
}

// connectRequirement 返回业务方法是否可用；ok=false 表示该方法
// 在本后台未实现（明确拒绝，不静默）。能力声明已删除，不做 requirements 门禁。
func connectRequirement(method string) (requirement string, ok bool) {
	switch {
	case method == "cipher.encrypt" || method == "cipher.decrypt":
		return "", true
	case method == "identity.get" || method == "intent.sign":
		return "", true
	case method == "payment.pay" || method == "payment.status":
		return "", true
	case method == "channel.publish" || method == "channel.subscription_set":
		return "", true
	case method == "msfile.stat" || method == "msfile.seed.read" || method == "msfile.block.read":
		return "", true
	default:
		return "", false
	}
}

func (d *Daemon) opConnectApprovalsList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	d.sweepExpiredApprovals()
	list := d.connectMgr.Queue().List()
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, a.View())
	}
	return map[string]any{"approvals": out}, nil
}

// opConnectBindingsList 返回当前在线的应用连接视图（未关闭且未暂停的实际运行连接）。
// 连接由登录/启动/恢复建立，退出/撤销关闭，暂停保留授权；不用持久授权条数代替。
func (d *Daemon) opConnectBindingsList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	byID := map[string]connect.Auth{}
	for _, a := range d.connectMgr.AuthStore().List() {
		byID[a.AuthorizationID] = a
	}
	gen := d.connectMgr.Generation()
	out := make([]map[string]any, 0)
	for _, c := range d.connectMgr.Connections().Online() {
		a, ok := byID[c.AuthID]
		if !ok || a.Generation == "" || a.Generation != gen {
			continue
		}
		out = append(out, map[string]any{
			"id":     a.AuthorizationID,
			"origin": a.Origin, "network": a.Network, "generation": a.Generation,
			"applicationKey": connect.ApplicationKey(a.AppPublicKeyHex),
		})
	}
	return map[string]any{"bindings": out}, nil
}

func (d *Daemon) opConnectAuthList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	auths := d.connectMgr.AuthStore().List()
	gen := d.connectMgr.Generation()
	out := make([]map[string]any, 0, len(auths))
	for _, a := range auths {
		if a.Generation == "" || a.Generation != gen {
			continue
		}
		out = append(out, map[string]any{
			"network": a.Network, "origin": a.Origin, "generation": a.Generation,
			"createdAt": a.CreatedAt, "authorizationId": a.AuthorizationID,
			"applicationKey": connect.ApplicationKey(a.AppPublicKeyHex),
		})
	}
	return map[string]any{"authorizations": out}, nil
}

func (d *Daemon) opConnectAuthRevoke(ctx context.Context, p kmapi.ConnectAuthRevokeRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	key := connect.ApplicationKey(p.AppPublicKeyHex)
	if key == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "verified App public key required")
	}
	var revErr error
	d.connectMgr.SerializedAuthChange(func() {
		revErr = d.connectMgr.AuthStore().RevokeApp(key)
		if revErr == nil {
			d.connectMgr.Connections().CloseByApp(key)
		}
	})
	if revErr != nil {
		return nil, errDetail(kmapi.ErrNotFound, revErr.Error())
	}
	_, _ = d.connectMgr.History().Append(connect.HistoryEvent{AppKey: key, Kind: "disconnected", Detail: "disconnect-app"})
	d.publishConnect()
	return kmapi.Ok{Ok: true}, nil
}

// opConnectApprovalsDecide 只做终态转换，不执行原请求。
// 批准后的执行由 connect.request.exec（approvalId 轮询）认领一次并返回结果，
// 避免决定响应塞不下二进制附件，也避免重复执行。
// 只有批准时才可保存自动许可（grantAuto）；拒绝/取消不保存。
// 许可写入失败时返回 INTERNAL，不显示“已开启自动同意”。
func (d *Daemon) opConnectApprovalsDecide(ctx context.Context, p kmapi.ConnectApprovalDecideRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if strings.TrimSpace(p.ApprovalID) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "approvalId 必填")
	}
	// 必须为钱包管理上下文（Web 钱包界面），App 上下文禁止决定。
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权决定审批")
	}
	d.sweepExpiredApprovals()
	if p.GrantAuto != nil && *p.GrantAuto && p.Decision != "approve" {
		return nil, errDetail(kmapi.ErrBadRequest, "只有批准时才可保存自动许可")
	}
	// 批准并授予自动许可走统一串行路径：原授权复核＋pending→approved＋许可提交
	// 在授权生命周期锁内按统一顺序完成，撤销/替换（同样经该锁）与本段互斥：
	// 撤销先到则复核失败、旧待办被终结且不授予；授予先到则撤销随后只删授权、
	// 保留此次明确授予的许可（断开与取消许可是分离的操作）。失败不留下可执行状态。
	if p.GrantAuto != nil && *p.GrantAuto && p.Decision == "approve" {
		pre, ok := d.connectMgr.Queue().Get(p.ApprovalID)
		if !ok {
			return nil, errDetail(kmapi.ErrNotFound, "审批不存在")
		}
		if pre.Status == "staged" {
			return nil, errDetail(kmapi.ErrBadRequest, "审批尚未发布")
		}
		if pre.Status == "pending" {
			if !connect.IsApprovalMethod(pre.Method) {
				return nil, errDetail(kmapi.ErrBadRequest, "该操作不支持自动许可")
			}
			var approval connect.Approval
			var first bool
			var derr error
			// 复核与提交同处授权生命周期临界区：与撤销/替换按统一顺序串行，
			// 杜绝“先校验后撤销”窗口；锁顺序为 authMu → queue → {authstore, appstate}。
			d.connectMgr.SerializedAuthChange(func() {
				if strings.TrimSpace(pre.AuthorizationID) != "" {
					if _, verr := d.validateConnectAuth(pre.AuthorizationID, pre.Origin); verr != nil {
						// 原授权已撤销/失效：终结该旧待办，不授予许可。
						if cancelled, _, cerr := d.connectMgr.Queue().Decide(p.ApprovalID, "cancel"); cerr == nil {
							if cancelled.Method == "payment.pay" {
								d.rejectPaymentByApproval(cancelled, "auth-invalid")
							}
							d.recordApprovalHistory(cancelled, "user-canceled", false, "auth-invalid")
							d.publishConnect()
						}
						derr = verr
						return
					}
				}
				var gerr error
				approval, first, gerr = d.connectMgr.Queue().DecideApproveWithGrant(p.ApprovalID, func(a connect.Approval) error {
					if perr := d.grantAutoPermission(a.AppPublicKeyHex, a.Origin, a.Method); perr != nil {
						return fmt.Errorf("%s", perr.Message)
					}
					return nil
				})
				if gerr != nil {
					derr = gerr
				}
			})
			if derr != nil {
				if derr.Error() == "审批已过期" {
					d.sweepExpiredApprovals()
					return nil, errDetail(kmapi.ErrStateConflict, "审批已过期")
				}
				if ed, ok := derr.(*kmapi.ErrorDetail); ok {
					return nil, ed
				}
				d.publishConnect()
				return nil, errDetail(kmapi.ErrInternal, "自动许可保存失败")
			}
			if !first {
				return map[string]any{"ok": true, "status": approval.Status, "deduped": true}, nil
			}
			d.recordApprovalHistory(approval, "manual-approved", false, "")
			d.publishConnect()
			return map[string]any{"ok": true, "status": approval.Status}, nil
		}
		// 已终态：走下方的重复决定路径（幂等），不再重复授予。
	}
	approval, first, err := d.connectMgr.Queue().Decide(p.ApprovalID, p.Decision)
	if err != nil {
		if err.Error() == "审批已过期" {
			d.sweepExpiredApprovals()
			return nil, errDetail(kmapi.ErrStateConflict, "审批已过期")
		}
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	if !first {
		// 重复决定不重复执行。但“已批准、尚未认领”的审批可能因连接已死
		// 永远无人认领：此时取消/拒绝决定将其原子终结并释放冻结，
		// 已认领或已终态的不动（认领是执行的唯一入口，未认领即确定未提交）。
		if (p.Decision == "cancel" || p.Decision == "reject") && approval.Status == "approved" {
			if abandoned, ok := d.connectMgr.Queue().AbandonApproved(p.ApprovalID); ok {
				if abandoned.Method == "payment.pay" {
					d.rejectPaymentByApproval(abandoned, "abandoned")
				}
				d.recordApprovalHistory(abandoned, "user-canceled", false, "abandoned")
				d.publishConnect()
				return map[string]any{"ok": true, "status": abandoned.Status}, nil
			}
		}
		return map[string]any{"ok": true, "status": approval.Status, "deduped": true}, nil
	}
	if approval.Status == "approved" {
		d.recordApprovalHistory(approval, "manual-approved", false, "")
	} else if approval.Status == "rejected" {
		d.recordApprovalHistory(approval, "user-canceled", false, "user-rejected")
	} else if approval.Status == "cancelled" {
		d.recordApprovalHistory(approval, "user-canceled", false, "cancelled")
	}
	// 付款拒绝/取消：明确未提交，释放冻结输入，不广播。
	if approval.Method == "payment.pay" && (approval.Status == "rejected" || approval.Status == "cancelled") {
		reason := "user-rejected"
		if approval.Status == "cancelled" {
			reason = "cancelled"
		}
		d.rejectPaymentByApproval(approval, reason)
	}
	d.publishConnect()
	return map[string]any{"ok": true, "status": approval.Status}, nil
}

// validateConnectAuth 校验运行期授权：存在、来源一致、代际有效、网络/钱包一致。
// 授权 ID 本身不可猜测；公开的 App 公钥不能替代授权检查。
func (d *Daemon) validateConnectAuth(authorizationID, origin string) (connect.Auth, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if strings.TrimSpace(authorizationID) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrBadRequest, "authorizationId 必填")
	}
	if strings.TrimSpace(origin) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrBadRequest, "origin 缺失")
	}
	a, ok := d.connectMgr.AuthStore().FindByID(authorizationID)
	if !ok {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	if a.Origin != origin {
		return connect.Auth{}, errDetail(kmapi.ErrBadRequest, "来源不匹配")
	}
	// 代际必须精确匹配：缺失代际的旧记录同样失效，不因字段为空而放行。
	if connect.ApplicationKey(a.AppPublicKeyHex) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrOriginRejected, "verified App identity required")
	}
	if a.DeploymentID != "" {
		r, ok := d.deployments.Identity(a.Origin)
		if !ok || r.DeploymentID != a.DeploymentID || r.AppPublicKeyHex != a.AppPublicKeyHex || r.Generation != a.DeploymentGeneration {
			return connect.Auth{}, errDetail(kmapi.ErrNotFound, "deployment no longer active")
		}
	}
	if a.Generation == "" || a.Generation != d.connectMgr.Generation() {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已过期，请重新登录")
	}
	if !d.isInitialized() {
		return connect.Auth{}, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	walletPub := d.publicKey
	d.mu.Unlock()
	if a.Network != info.BsvNetwork || !strings.EqualFold(a.WalletPublicKey, walletPub) {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	// 旧授权不能补建运行连接；只有新登录或启动才能登记在线连接。
	if d.connectMgr.Connections() == nil || !d.connectMgr.Connections().IsOnline(a.AuthorizationID) {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "Connection closed; login again")
	}
	d.connectMgr.Connections().Touch(a.AuthorizationID)
	return a, nil
}

// lookupValidAuth 复核授权本身（不含连接在线检查），仅供撤销授权等不执行业务的路径使用。
func (d *Daemon) lookupValidAuth(authorizationID, origin string) (connect.Auth, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if strings.TrimSpace(authorizationID) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrBadRequest, "authorizationId 必填")
	}
	if strings.TrimSpace(origin) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrBadRequest, "origin 缺失")
	}
	a, ok := d.connectMgr.AuthStore().FindByID(authorizationID)
	if !ok {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	if strings.TrimSpace(a.Origin) == "" || strings.TrimSpace(a.Network) == "" || strings.TrimSpace(a.WalletPublicKey) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	if a.Origin != origin {
		return connect.Auth{}, errDetail(kmapi.ErrBadRequest, "来源不匹配")
	}
	if connect.ApplicationKey(a.AppPublicKeyHex) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrOriginRejected, "verified App identity required")
	}
	if a.DeploymentID != "" {
		r, ok := d.deployments.Identity(a.Origin)
		if !ok || r.DeploymentID != a.DeploymentID || r.AppPublicKeyHex != a.AppPublicKeyHex || r.Generation != a.DeploymentGeneration {
			return connect.Auth{}, errDetail(kmapi.ErrNotFound, "deployment no longer active")
		}
	}
	if a.Generation == "" || a.Generation != d.connectMgr.Generation() {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已过期，请重新登录")
	}
	if !d.isInitialized() {
		return connect.Auth{}, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	walletPub := d.publicKey
	d.mu.Unlock()
	if a.Network != info.BsvNetwork || !strings.EqualFold(a.WalletPublicKey, walletPub) {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已失效")
	}
	return a, nil
}

// recheckAuthLive 在不可逆提交点前轻量复核授权记录仍然存在、身份完整且
// 运行代际一致（不重读网络快照；完整复核由执行入口的 validate 完成）。
// 用于签名/广播前缩小“校验后撤销”窗口；失败按授权失效处理。
func (d *Daemon) recheckAuthLive(authorizationID string) (connect.Auth, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return connect.Auth{}, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	a, ok := d.connectMgr.AuthStore().FindByID(authorizationID)
	if !ok {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	if a.Origin == "" || a.Network == "" || a.WalletPublicKey == "" {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	if connect.ApplicationKey(a.AppPublicKeyHex) == "" {
		return connect.Auth{}, errDetail(kmapi.ErrOriginRejected, "verified App identity required")
	}
	if a.DeploymentID != "" {
		r, ok := d.deployments.Identity(a.Origin)
		if !ok || r.DeploymentID != a.DeploymentID || r.AppPublicKeyHex != a.AppPublicKeyHex || r.Generation != a.DeploymentGeneration {
			return connect.Auth{}, errDetail(kmapi.ErrNotFound, "deployment no longer active")
		}
	}
	if a.Generation == "" || a.Generation != d.connectMgr.Generation() {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已过期，请重新登录")
	}
	return a, nil
}

func (d *Daemon) opConnectExec(ctx context.Context, raw json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	var p kmapi.ConnectExecRequest
	if len(raw) > 0 {
		if err := decodeStrict(raw, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
	}
	// 已审批执行的认领：approvalId 指向已批准的审批，执行一次并返回结果。
	// 重复认领返回已保存的结果，不重复执行。多页同时批准仅执行一次（条件转换保证）。
	if p.ApprovalID != nil && *p.ApprovalID != "" && p.Method == nil {
		d.sweepExpiredApprovals()
		a, ok := d.connectMgr.Queue().Get(*p.ApprovalID)
		if !ok {
			return nil, nil, errDetail(kmapi.ErrNotFound, "审批不存在")
		}
		if a.Status != "approved" {
			return map[string]any{"approvalId": a.ID, "status": a.Status}, nil, nil
		}
		if res, binaries, code, msg, ok := d.connectMgr.Queue().CompletedResult(a.ID); ok {
			if code != "" {
				return nil, nil, errDetail(code, msg)
			}
			return res, binaries, nil
		}
		claimed, ok := d.connectMgr.Queue().ClaimExecution(a.ID)
		if !ok {
			// 竞争失败后必须重新读取终态：可能是另一个轮询已完成，也可能仍在执行。
			if res, binaries, code, msg, ok := d.connectMgr.Queue().CompletedResult(a.ID); ok {
				if code != "" {
					return nil, nil, errDetail(code, msg)
				}
				return res, binaries, nil
			}
			return nil, nil, errDetail(kmapi.ErrStateConflict, "审批正在执行")
		}
		res, binaries, perr := d.execConnectMethod(ctx, claimed)
		if perr != nil {
			// 失败也要缓存成明确终态，重复查询不能变成空成功；区分人工与自动、业务失败。
			d.connectMgr.Queue().StoreError(a.ID, perr.Code, perr.Message)
			d.recordApprovalHistory(a, "failed", isAuto(ctx), perr.Code)
			d.publishConnect()
			// 超时与取消后的附件清理：失败终态丢弃请求附件引用，避免无限占用内存。
			// （StoreError 保留错误码，ReqBinaries 由后台异步清理；此处不删终态记录。）
			return nil, nil, perr
		}
		d.connectMgr.Queue().StoreResult(a.ID, res, binaries)
		d.recordApprovalHistory(a, "executed", isAuto(ctx), "")
		// 登录执行成功即建立连接：记录 connected 事件。
		if a.Method == "connect.login" {
			d.recordApprovalHistory(a, "connected", false, "")
		}
		d.publishConnect()
		return res, binaries, nil
	}
	method := ""
	if p.Method != nil {
		method = *p.Method
	}
	if method == "" {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "method 必填")
	}
	origin := ""
	if p.Origin != nil {
		origin = *p.Origin
	}

	// launch 由钱包主动启动流程消费一次性令牌：发起即授权，不进入审批队列。
	// 普通 connect.login 继续走审批，不能借此变成免审批登录。
	if method == "connect.launch" {
		return d.execConnectLaunch(ctx, p)
	}
	// 建立连接类方法：每次调用都需用户确认绑定，不做静默自动执行。
	// 可信来源属于 Receiver 上下文；应用不得自报 origin 冒用他人权限，
	// 后台继续核对来源与原授权一致。
	if isEstablishingConnectMethod(method) {
		if strings.TrimSpace(origin) == "" {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "origin 缺失")
		}
		canonical, cerr := connect.CanonicalOrigin(strings.TrimSpace(origin))
		if cerr != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "origin 必须为精确 HTTP(S) origin")
		}
		paramsMap := map[string]any{}
		if p.Params != nil {
			b, _ := json.Marshal(p.Params)
			_ = json.Unmarshal(b, &paramsMap)
		}
		appHex, deploymentID, identityErr := d.verifiedConnectIdentity(paramsMap, canonical)
		if identityErr != nil {
			return nil, nil, errDetail(kmapi.ErrOriginRejected, identityErr.Error())
		}
		a := d.connectMgr.Queue().CreateFull("", "", method, paramsMap, canonical, nil, appHex, deploymentID, d.deploymentGeneration(canonical))
		d.publishConnect()
		return map[string]any{"approvalId": a.ID, "status": "pending"}, nil, nil
	}

	// 登出：需有效授权，自动执行（撤销），不经审批队列。
	// 断开连接与取消持久操作许可分开：此处仅撤销连接授权，不删历史与资金责任。
	if method == "connect.logout" {
		var authorizationID string
		if p.AuthorizationID != nil {
			authorizationID = *p.AuthorizationID
		}
		a, perr := d.lookupValidAuth(authorizationID, origin)
		if perr != nil {
			return nil, nil, perr
		}
		var logoutErr error
		d.connectMgr.SerializedAuthChange(func() {
			logoutErr = d.connectMgr.AuthStore().RevokeByID(authorizationID)
			if logoutErr == nil && d.connectMgr.Connections() != nil {
				d.connectMgr.Connections().Close(authorizationID)
			}
		})
		if logoutErr != nil {
			return nil, nil, errDetail(kmapi.ErrNotFound, "授权不存在")
		}
		appKey := connect.ApplicationKey(a.AppPublicKeyHex)
		_, _ = d.connectMgr.History().Append(connect.HistoryEvent{
			AppKey: appKey, Origin: a.Origin,
			Kind: "disconnected", Detail: "logout",
		})
		d.publishConnect()
		return map[string]any{"revokedAt": time.Now().UnixMilli()}, nil, nil
	}

	// 其余业务方法：必须持有有效授权，不接受裸 proof；公开 proof 不能替代授权。
	var authorizationID string
	if p.AuthorizationID != nil {
		authorizationID = *p.AuthorizationID
	}
	auth, perr := d.validateConnectAuth(authorizationID, origin)
	if perr != nil {
		return nil, nil, perr
	}
	_, supported := connectRequirement(method)
	if !supported {
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
	// 统一流程：校验参数生成不可变快照 → 查自动许可 → 无许可建待办，有许可走同一业务路径。
	paramsSnap := d.paramsMapOf(p)
	transportReqID := ""
	if p.RequestID != nil {
		transportReqID = *p.RequestID
	}
	if connect.IsApprovalMethod(method) {
		snap, verr := validateBusinessParams(method, paramsSnap)
		if verr != nil {
			return nil, nil, verr
		}
		hasAuto, aerr := d.hasAutoPermission(auth.AppPublicKeyHex, method)
		if aerr != nil {
			return nil, nil, aerr
		}
		if hasAuto {
			// 自动许可：进入同一业务执行路径，直接执行并记录自动历史。
			autoCtx := withAuto(WithAppCaller(ctx))
			res, bins, perr := d.execApprovedBusiness(autoCtx, auth, method, snap, attachments, origin, transportReqID)
			if perr != nil {
				d.recordApprovalHistory(connect.Approval{
					ID:     "",
					Method: method, Origin: origin, AppPublicKeyHex: auth.AppPublicKeyHex,
				}, "failed", true, perr.Code)
				return nil, nil, perr
			}
			d.recordApprovalHistory(connect.Approval{
				ID:     "",
				Method: method, Origin: origin, AppPublicKeyHex: auth.AppPublicKeyHex,
			}, "executed", true, "")
			// 人工与自动使用不同提醒：自动执行经 connect 事件通知前端合并提醒。
			d.publishConnect()
			return res, bins, nil
		}
		// 无许可：创建待办。付款沿用冻结+持久绑定路径；其余直接建 pending。
		if method == "payment.pay" {
			appCtx := WithAppCaller(ctx)
			res, _, perr := d.opConnectPaymentPayWithAuth(appCtx, auth, snap, origin, transportReqID)
			if perr != nil {
				return nil, nil, perr
			}
			d.publishConnect()
			return res, nil, nil
		}
		// 其余四类：一次装配完整待办（原授权引用、原传输请求 ID、期限、参数快照与
		// 二进制附件同一锁内深拷贝），返回前即进入可批准队列，无“先可见后补齐”窗口。
		appr := d.connectMgr.Queue().CreateFull(auth.AuthorizationID, transportReqID, method, snap, origin, attachments, auth.AppPublicKeyHex, auth.DeploymentID, auth.DeploymentGeneration)
		d.publishConnect()
		return map[string]any{"approvalId": appr.ID, "status": "pending"}, nil, nil
	}
	appCtx := WithAppCaller(ctx)
	switch {
	case method == "payment.status":
		res, perr := d.opConnectPaymentStatus(appCtx, auth, paramsSnap, origin)
		if perr != nil {
			return nil, nil, perr
		}
		return res, nil, nil
	default:
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
}

// isExactHttpOrigin 只接受精确 HTTP(S) origin，不接受路径、查询或 opaque 来源。
func isExactHttpOrigin(origin string) bool {
	_, err := connect.CanonicalOrigin(origin)
	return err == nil
}

// opConnectLaunchIssue 为钱包主动启动签发一次性令牌。
// 钱包管理上下文专用，App 上下文禁止调用；签发时钱包必须已初始化并解锁。
func (d *Daemon) opConnectLaunchIssue(ctx context.Context, p kmapi.ConnectLaunchIssueRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权签发启动令牌")
	}
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if e := d.needUnlocked(); e != nil {
		return nil, e
	}
	origin := strings.TrimSpace(p.Origin)
	canonical, cerr := connect.CanonicalOrigin(origin)
	if cerr != nil {
		return nil, errDetail(kmapi.ErrBadRequest, "origin 必须为精确 HTTP(S) origin")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	walletPub := d.publicKey
	d.mu.Unlock()
	// 钱包主动启动即用户授权：由钱包已确认的目标 URL 推导 origin 并绑定，
	// 签发一次性令牌；不抓取 HTML proof、不做签名信任根与能力预检。
	if expected := stringOrEmpty(p.ExpectedAppPublicKeyHex); expected != "" && connect.ApplicationKey(expected) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "invalid expected App public key")
	}
	if target := stringOrEmpty(p.URL); target != "" {
		_, actualOrigin, e := connect.CanonicalEntryURL(target)
		if e != nil || actualOrigin != canonical {
			return nil, errDetail(kmapi.ErrBadRequest, "launch URL origin mismatch")
		}
	}
	if id := stringOrEmpty(p.DeploymentID); id != "" {
		active, ok := d.deployments.Identity(canonical)
		if !ok || active.DeploymentID != id {
			return nil, errDetail(kmapi.ErrOriginRejected, "deployment no longer active")
		}
	} else if _, ok := d.deployments.Identity(canonical); ok {
		return nil, errDetail(kmapi.ErrOriginRejected, "Files launch requires active deployment context")
	}
	rec, err := d.connectMgr.LaunchStore().Issue(canonical, info.BsvNetwork, walletPub, d.connectMgr.Generation(), connect.LaunchTTL, stringOrEmpty(p.ExpectedAppPublicKeyHex), stringOrEmpty(p.DeploymentID), stringOrEmpty(p.URL), d.deploymentGeneration(canonical))
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, "签发启动令牌失败")
	}
	return map[string]any{
		"launchToken": rec.Token,
		"expiresAt":   rec.ExpiresAt,
	}, nil
}

// execConnectLaunch 消费钱包签发的一次性令牌并直接签发授权。
// 用户在钱包点击启动即视为同意：不进入审批队列，不弹第二次登录审批。
// 令牌先消费再校验，重放、错误来源、身份或权限不匹配、过期或运行代际失效一律拒绝。
// 授权的身份与许可范围以签发时绑定的为准，消费 proof 仅作一致性核对。
func (d *Daemon) execConnectLaunch(ctx context.Context, p kmapi.ConnectExecRequest) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	paramsMap := map[string]any{}
	if p.Params != nil {
		b, _ := json.Marshal(p.Params)
		_ = json.Unmarshal(b, &paramsMap)
	}
	token, _ := paramsMap["launchToken"].(string)
	if strings.TrimSpace(token) == "" {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "launchToken 必填")
	}
	origin := ""
	if p.Origin != nil {
		origin = *p.Origin
	}
	canonical, cerr := connect.CanonicalOrigin(strings.TrimSpace(origin))
	if cerr != nil {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "origin 必须为精确 HTTP(S) origin")
	}
	rec, ok := d.connectMgr.LaunchStore().Consume(strings.TrimSpace(token))
	if !ok {
		return nil, nil, errDetail(kmapi.ErrNotFound, "启动令牌无效或已使用")
	}
	if time.Now().UnixMilli() > rec.ExpiresAt {
		return nil, nil, errDetail(kmapi.ErrNotFound, "启动令牌已过期")
	}
	// 跨 origin 不能消费：旧 origin 的启动令牌、授权、数据均不可沿用。
	if canonical != rec.Origin {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "来源不匹配")
	}
	if !d.isInitialized() {
		return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	if d.isStopping() {
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	d.mu.Lock()
	locked := d.state != StateUnlocked
	walletPub := d.publicKey
	d.mu.Unlock()
	if locked {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	if rec.Generation == "" || rec.Generation != d.connectMgr.Generation() {
		return nil, nil, errDetail(kmapi.ErrNotFound, "启动上下文已失效，请从钱包重新打开")
	}
	if rec.Network != info.BsvNetwork || !strings.EqualFold(rec.WalletPublicKey, walletPub) {
		return nil, nil, errDetail(kmapi.ErrNotFound, "启动上下文已失效")
	}
	now := time.Now().UnixMilli()
	authorizationID, idErr := newAuthorizationID()
	if idErr != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, "生成授权失败")
	}
	// 同一来源允许多个独立授权并存（各连接独立授权、独立撤销）：
	// 启动新窗口不撤销该来源既有授权、不关闭既有连接；关闭按连接保留
	// 授权，显式退出按 authorizationId 撤销本连接，整来源断开仍走
	// connect.auth.revoke（按 App 公钥全撤）。
	// 启动消费 proof 仅作一致性核对：提交的证明须与令牌绑定的来源一致，
	// 不一致直接拒绝；身份必须来自有效证明或本地可信部署登记。
	launchAppHex, deploymentID, identityErr := d.verifiedConnectIdentity(paramsMap, canonical)
	if identityErr != nil {
		return nil, nil, errDetail(kmapi.ErrOriginRejected, identityErr.Error())
	}
	if rec.ExpectedAppPublicKeyHex != "" && rec.ExpectedAppPublicKeyHex != launchAppHex {
		return nil, nil, errDetail(kmapi.ErrOriginRejected, "App public key differs from directory target")
	}
	if rec.DeploymentID != deploymentID || rec.DeploymentGeneration != d.deploymentGeneration(canonical) {
		return nil, nil, errDetail(kmapi.ErrOriginRejected, "deployment launch context mismatch")
	}
	savedAuth := connect.Auth{
		AuthorizationID: authorizationID,
		Origin:          canonical, Network: info.BsvNetwork, WalletPublicKey: walletPub,
		Generation:      d.connectMgr.Generation(),
		AppPublicKeyHex: launchAppHex, DeploymentID: deploymentID, DeploymentGeneration: rec.DeploymentGeneration,
	}
	display, displayErr := d.verifiedConnectDisplay(paramsMap, canonical)
	if displayErr != nil {
		return nil, nil, errDetail(kmapi.ErrOriginRejected, displayErr.Error())
	}
	if err := d.connectMgr.Directory().RememberDisplay(launchAppHex, display); err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, "保存应用资料失败")
	}
	// 目录事务先提交；失败时尚未保存授权或登记活动连接。
	if rec.URL != "" {
		if err := d.connectMgr.Directory().ResolveURL(rec.URL, launchAppHex, display); err != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
		}
	}
	var launchSaveErr error
	d.connectMgr.SerializedAuthChange(func() {
		launchSaveErr = d.connectMgr.AuthStore().Save(savedAuth)
		if launchSaveErr == nil && d.connectMgr.Connections() != nil {
			if !d.registerConnectConnection(ctx, savedAuth) {
				launchSaveErr = context.Canceled
			}
		}
	})
	if launchSaveErr != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, "保存授权失败")
	}
	appKey := launchAppHex
	_, _ = d.connectMgr.History().Append(connect.HistoryEvent{
		AppKey: appKey, Origin: canonical,
		Kind: "connected", Detail: "launch",
	})

	d.publishConnect()
	return map[string]any{
		"authorizationId": authorizationID,
		"networkIdentity": map[string]any{
			"bsvNetwork": info.BsvNetwork, "publicKeyHex": walletPub,
		},
		"resolvedClaims": map[string]any{},
		"resolvedAt":     now,
	}, nil, nil
}

func (d *Daemon) paramsMapOf(p kmapi.ConnectExecRequest) map[string]any {
	paramsMap := map[string]any{}
	if p.Params != nil {
		b, _ := json.Marshal(p.Params)
		_ = json.Unmarshal(b, &paramsMap)
	}
	return paramsMap
}

// execConnectMethod 执行已批准审批保存的原请求。只被 approvalId 认领路径调用一次。
// 执行前再次复核原授权和生命周期；保存实际执行结果，区分人工/自动与业务失败。
func (d *Daemon) execConnectMethod(ctx context.Context, a connect.Approval) (any, map[string][]byte, *kmapi.ErrorDetail) {
	switch {
	case a.Method == "payment.pay":
		return d.execPaymentApproved(ctx, a)
	case a.Method == "identity.get":
		return d.execIdentityGet(ctx, a)
	case a.Method == "intent.sign":
		return d.execIntentSign(ctx, a)
	case a.Method == "cipher.encrypt" || a.Method == "cipher.decrypt":
		return d.execCipherApproved(ctx, a)
	case a.Method == "connect.login":
		if !d.isInitialized() {
			return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
		}
		if d.isStopping() {
			return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
		}
		info, _, err := d.store.Snapshot()
		if err != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
		}
		now := time.Now().UnixMilli()
		authorizationID, idErr := newAuthorizationID()
		if idErr != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, "生成授权失败")
		}
		d.mu.Lock()
		walletPub := d.publicKey
		d.mu.Unlock()
		// 同一来源允许多个独立授权并存（各连接独立授权、独立撤销）：
		// 重新登录只新增本连接授权，不撤销该来源既有授权、不关闭既有连接。
		// 建连时验证发布身份：审批参数可能携带 SDK 提交的 URL 来源证明；
		// 验签并核对可信来源一致后记入授权快照。证明无效直接拒绝，不创建授权；
		// 缺少有效 URL 证明或可信部署登记时拒绝创建正式授权。
		loginAppHex := a.AppPublicKeyHex
		if connect.ApplicationKey(loginAppHex) == "" {
			return nil, nil, errDetail(kmapi.ErrOriginRejected, "verified App identity required")
		}
		if a.DeploymentID != "" {
			r, ok := d.deployments.Identity(a.Origin)
			if !ok || r.DeploymentID != a.DeploymentID || r.AppPublicKeyHex != loginAppHex || r.Generation != a.DeploymentGeneration {
				return nil, nil, errDetail(kmapi.ErrOriginRejected, "deployment no longer active")
			}
		}
		display, displayErr := d.verifiedConnectDisplay(a.Params, a.Origin)
		if displayErr != nil {
			return nil, nil, errDetail(kmapi.ErrOriginRejected, "应用资料验证失败")
		}
		if err := d.connectMgr.Directory().RememberDisplay(loginAppHex, display); err != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, "保存应用资料失败")
		}
		savedLoginAuth := connect.Auth{
			AuthorizationID: authorizationID,
			Origin:          a.Origin, Network: info.BsvNetwork, WalletPublicKey: walletPub,
			Generation:      d.connectMgr.Generation(),
			AppPublicKeyHex: loginAppHex, DeploymentID: a.DeploymentID, DeploymentGeneration: a.DeploymentGeneration,
		}
		var loginSaveErr error
		d.connectMgr.SerializedAuthChange(func() {
			loginSaveErr = d.connectMgr.AuthStore().Save(savedLoginAuth)
			if loginSaveErr == nil && d.connectMgr.Connections() != nil {
				if !d.registerConnectConnection(ctx, savedLoginAuth) {
					loginSaveErr = context.Canceled
				}
			}
		})
		if loginSaveErr != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, "保存授权失败")
		}
		return map[string]any{
			"authorizationId": authorizationID,
			"networkIdentity": map[string]any{
				"bsvNetwork": info.BsvNetwork, "publicKeyHex": walletPub,
			},
			"resolvedClaims": map[string]any{},
			"resolvedAt":     now,
		}, nil, nil
	default:
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
}

// newAuthorizationID 生成不可猜测的授权引用；授权本身即运行期凭证，
// 不能从审批编号或时间等公开信息推导。
func newAuthorizationID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "auth_" + hex.EncodeToString(b), nil
}

func toInt64(v any) (int64, bool) {
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
	default:
		return 0, false
	}
}

func (d *Daemon) execConnectCipher(ctx context.Context, auth connect.Auth, method string, params map[string]any, attachments map[string][]byte, origin string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if !d.isInitialized() {
		return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	if d.isStopping() {
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	// cipher 密钥必须源自钱包秘密材料：未解锁时拒绝，绝不以降级方式执行。
	d.mu.Lock()
	priv := append([]byte(nil), d.privateKey...)
	pub := d.publicKey
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked || len(priv) == 0 {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	// 已验证 App 身份的授权按 App 公钥派生 cipher 密钥（origin 不再进入派生）；
	// 未验证传输沿用来源绑定派生，保持旧行为，不静默升级。
	siteKey, derr := connect.DeriveAppCipherKey(priv, pub, info.BsvNetwork, auth.AppPublicKeyHex)
	err = derr
	for i := range priv {
		priv[i] = 0
	}
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "派生失败")
	}
	defer func() {
		for i := range siteKey {
			siteKey[i] = 0
		}
	}()
	switch method {
	case "cipher.encrypt":
		content, ok := attachments["content"]
		if !ok {
			for _, v := range attachments {
				content = v
				ok = true
				break
			}
		}
		if !ok {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "附件缺失")
		}
		ctype, _ := params["contentType"].(string)
		nonce, ct, err := connect.CipherEncrypt(siteKey, ctype, content)
		if err != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
		}
		return map[string]any{}, map[string][]byte{"nonce": nonce, "cipherbytes": ct}, nil
	case "cipher.decrypt":
		nonce, ok1 := attachments["nonce"]
		ct, ok2 := attachments["cipherbytes"]
		if !ok1 || !ok2 {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "附件缺失")
		}
		plain, ctype, err := connect.CipherDecrypt(siteKey, nonce, ct)
		if err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "Decrypt failed")
		}
		return map[string]any{"contentType": ctype}, map[string][]byte{"content": plain}, nil
	default:
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
}
