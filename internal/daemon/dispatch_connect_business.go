package daemon

import (
	"context"
	"encoding/hex"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/connectenvelope"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

// execIdentityGet 执行已批准的身份凭证：复核原授权与生命周期后签名 CBOR 信封 v2。
// 信封为 [2, requestId(approvalId), aud, iat, exp, text, pubkeyBytes, projection, network]。
func (d *Daemon) execIdentityGet(ctx context.Context, a connect.Approval) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.store == nil {
		return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	if d.isStopping() {
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	// 执行前再次复核原授权和生命周期，不借新授权继续旧申请。
	auth, perr := d.validateConnectAuth(a.AuthorizationID, a.Origin)
	if perr != nil {
		return nil, nil, perr
	}
	if !strings.EqualFold(auth.AppPublicKey, a.AppPublicKey) || auth.AppID != a.AppID || !strings.EqualFold(auth.IdentityDigest, a.IdentityDigest) {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "App 身份与原授权不一致")
	}
	params := a.Params
	aud, _ := params["aud"].(string)
	if aud != a.Origin {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "aud 与来源不一致")
	}
	iat, ok1 := toInt64Any(params["iat"])
	exp, ok2 := toInt64Any(params["exp"])
	if !ok1 || !ok2 || exp <= iat {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "iat/exp 非法")
	}
	text, _ := params["text"].(string)
	var claims []string
	if raw, ok := params["claims"]; ok {
		if arr, ok := raw.([]any); ok {
			for _, c := range arr {
				if s, ok := c.(string); ok {
					claims = append(claims, s)
				}
			}
		}
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	priv := append([]byte(nil), d.privateKey...)
	pub := d.publicKey
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked || len(priv) == 0 {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	defer func() {
		for i := range priv {
			priv[i] = 0
		}
	}()
	// 签名前提交点复核：授权在执行入口校验后被撤销则拒绝签名。
	if _, rerr := d.recheckAuthLive(a.AuthorizationID); rerr != nil {
		return nil, nil, rerr
	}
	envelope, sig, resolved, err := connectenvelope.BuildIdentitySigned(priv, a.EnvelopeRequestID(), aud, iat, exp, text, pub, info.BsvNetwork, claims)
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	pubBytes, _ := hex.DecodeString(pub)
	res := map[string]any{
		"resolvedClaims": resolved,
	}
	bins := map[string][]byte{
		"identityEnvelope": envelope,
		"signature":        sig,
		"subject":          pubBytes,
	}
	return res, bins, nil
}

// execIntentSign 执行已批准的意图签名：信封 v2 10 项，内容摘要必须与附件一致。
func (d *Daemon) execIntentSign(ctx context.Context, a connect.Approval) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.store == nil {
		return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	if d.isStopping() {
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	auth, perr := d.validateConnectAuth(a.AuthorizationID, a.Origin)
	if perr != nil {
		return nil, nil, perr
	}
	if !strings.EqualFold(auth.AppPublicKey, a.AppPublicKey) || auth.AppID != a.AppID || !strings.EqualFold(auth.IdentityDigest, a.IdentityDigest) {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "App 身份与原授权不一致")
	}
	params := a.Params
	aud, _ := params["aud"].(string)
	if aud != a.Origin {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "aud 与来源不一致")
	}
	iat, ok1 := toInt64Any(params["iat"])
	exp, ok2 := toInt64Any(params["exp"])
	if !ok1 || !ok2 || exp <= iat {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "iat/exp 非法")
	}
	text, _ := params["text"].(string)
	contentType, _ := params["contentType"].(string)
	// 空字节内容合法（SDK 允许零字节对象）：以键存在性判断缺失，不以长度判断。
	content, ok := a.ReqBinaries["content"]
	if !ok {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "附件缺失")
	}
	_ = content
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	priv := append([]byte(nil), d.privateKey...)
	pub := d.publicKey
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked || len(priv) == 0 {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	defer func() {
		for i := range priv {
			priv[i] = 0
		}
	}()
	// 签名前提交点复核：授权在执行入口校验后被撤销则拒绝签名。
	if _, rerr := d.recheckAuthLive(a.AuthorizationID); rerr != nil {
		return nil, nil, rerr
	}
	envelope, sig, err := connectenvelope.BuildIntentSigned(priv, a.EnvelopeRequestID(), aud, iat, exp, text, contentType, content, pub, info.BsvNetwork)
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	res := map[string]any{}
	bins := map[string][]byte{
		"signedEnvelope": envelope,
		"signature":      sig,
	}
	return res, bins, nil
}

// execCipherApproved 执行已批准的加解密：复核原授权后走同一业务路径。
func (d *Daemon) execCipherApproved(ctx context.Context, a connect.Approval) (any, map[string][]byte, *kmapi.ErrorDetail) {
	auth, perr := d.validateConnectAuth(a.AuthorizationID, a.Origin)
	if perr != nil {
		return nil, nil, perr
	}
	if !strings.EqualFold(auth.AppPublicKey, a.AppPublicKey) || auth.AppID != a.AppID {
		return nil, nil, errDetail(kmapi.ErrBadRequest, "App 身份与原授权不一致")
	}
	// 同一业务执行路径：直接复用现有加解密实现，附件来自审批快照。
	appCtx := WithAppCaller(ctx)
	if isAuto(ctx) {
		appCtx = withAuto(appCtx)
	}
	return d.execConnectCipher(appCtx, auth, a.Method, a.Params, a.ReqBinaries, a.Origin)
}
// execApprovedBusiness 为自动许可的直接执行入口：与审批认领走同一业务函数。
// 调用方已完成授权、能力、参数与许可校验；此处不再建待办，直接执行业务。
func (d *Daemon) execApprovedBusiness(ctx context.Context, auth connect.Auth, method string, snap map[string]any, attachments map[string][]byte, origin, requestID string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if requestID == "" {
		requestID = "auto"
	}
	switch method {
	case "identity.get":
		tmp := connect.Approval{
			ID: "auto-" + requestID, TransportRequestID: requestID, AppPublicKey: auth.AppPublicKey, AppID: auth.AppID, AppName: auth.AppName,
			Method: method, Params: snap, Origin: origin,
			Requirements: append([]string(nil), auth.Requirements...),
			IdentityDigest: auth.IdentityDigest, AuthorizationID: auth.AuthorizationID,
		}
		return d.execIdentityGet(withAuto(ctx), tmp)
	case "intent.sign":
		tmp := connect.Approval{
			ID: "auto-" + requestID, TransportRequestID: requestID, AppPublicKey: auth.AppPublicKey, AppID: auth.AppID, AppName: auth.AppName,
			Method: method, Params: snap, Origin: origin,
			Requirements: append([]string(nil), auth.Requirements...),
			IdentityDigest: auth.IdentityDigest, AuthorizationID: auth.AuthorizationID,
			ReqBinaries: copyBinaries(attachments),
		}
		return d.execIntentSign(withAuto(ctx), tmp)
	case "cipher.encrypt", "cipher.decrypt":
		tmp := connect.Approval{
			ID: "auto-" + requestID, TransportRequestID: requestID, AppPublicKey: auth.AppPublicKey, AppID: auth.AppID, AppName: auth.AppName,
			Method: method, Params: snap, Origin: origin,
			Requirements: append([]string(nil), auth.Requirements...),
			IdentityDigest: auth.IdentityDigest, AuthorizationID: auth.AuthorizationID,
			ReqBinaries: copyBinaries(attachments),
		}
		return d.execCipherApproved(withAuto(ctx), tmp)
	case "payment.pay":
		return d.execPaymentAuto(ctx, auth, snap, origin, requestID)
	default:
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
}

func copyBinaries(m map[string][]byte) map[string][]byte {
	if m == nil {
		return nil
	}
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// opConnectPaymentPayWithAuth 为手动路径的薄包装：与原冻结路径一致，仅确保审批携带原授权引用。
// 实际冻结逻辑仍在 opConnectPaymentPay 内（单次装配 staged 审批）。
// requestID 透传为审批的原传输请求 ID，供执行关联与排错。
func (d *Daemon) opConnectPaymentPayWithAuth(ctx context.Context, auth connect.Auth, params map[string]any, origin, requestID string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	return d.opConnectPaymentPay(ctx, auth, params, origin, requestID)
}

// execPaymentAuto 为自动许可的付款执行：复用同一冻结路径，随后立即批准并广播，返回最终结果而非 approvalId。
// 两层幂等保留：clientRequestId 绑定不可变业务申请；ApprovalID 条件转换保证一次执行。
func (d *Daemon) execPaymentAuto(ctx context.Context, auth connect.Auth, snap map[string]any, origin, requestID string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	res, _, perr := d.opConnectPaymentPay(WithAppCaller(ctx), auth, snap, origin, requestID)
	if perr != nil {
		return nil, nil, perr
	}
	// 已存在记录（幂等命中）直接返回最终结果，不新建审批。
	if m, ok := res.(map[string]any); ok {
		if _, hasApproval := m["approvalId"]; !hasApproval {
			return m, nil, nil
		}
		approvalID, _ := m["approvalId"].(string)
		if approvalID == "" {
			return m, nil, nil
		}
		// 自动批准：不经过用户面板，直接终态转换。
		approval, first, err := d.connectMgr.Queue().Decide(approvalID, "approve")
		if err != nil || !first {
			// 已被收敛或重复：按现有记录返回。
			if cur, ok := d.connectMgr.Queue().Get(approvalID); ok {
				if cur.Status != "approved" {
					return map[string]any{"approvalId": cur.ID, "status": cur.Status}, nil, nil
				}
				approval = cur
			} else {
				return nil, nil, errDetail(kmapi.ErrInternal, "付款审批状态异常")
			}
		}
		// 记录自动批准历史。
		d.recordApprovalHistory(approval, "auto-approved", true, "")
		claimed, ok := d.connectMgr.Queue().ClaimExecution(approvalID)
		if !ok {
			if res2, bins2, code, msg, ok2 := d.connectMgr.Queue().CompletedResult(approvalID); ok2 {
				if code != "" {
					return nil, nil, errDetail(code, msg)
				}
				return res2, bins2, nil
			}
			return nil, nil, errDetail(kmapi.ErrStateConflict, "审批正在执行")
		}
		autoCtx := withAuto(ctx)
		finalRes, bins, execErr := d.execConnectMethod(autoCtx, claimed)
		if execErr != nil {
			d.connectMgr.Queue().StoreError(approvalID, execErr.Code, execErr.Message)
			d.recordApprovalHistory(approval, "failed", true, execErr.Code)
			d.publishConnect()
			return nil, nil, execErr
		}
		d.connectMgr.Queue().StoreResult(approvalID, finalRes, bins)
		d.recordApprovalHistory(approval, "executed", true, "")
		d.publishConnect()
		return finalRes, bins, nil
	}
	return res, nil, nil
}
