package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appstorage"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

// 建立连接类方法：每次调用都需用户确认绑定，不做静默自动执行。
func isEstablishingConnectMethod(method string) bool {
	switch method {
	case "connect.login", "connect.resume", "connect.launch":
		return true
	default:
		return false
	}
}

// connectRequirement 返回执行业务方法所需的 App 许可；ok=false 表示该方法
// 在本后台未实现（明确拒绝，不静默）。
func connectRequirement(method string) (requirement string, ok bool) {
	switch {
	case strings.HasPrefix(method, "storage."):
		return "storage", true
	case method == "cipher.encrypt" || method == "cipher.decrypt":
		return "private-key", true
	default:
		return "", false
	}
}

func hasRequirement(auth connect.Auth, req string) bool {
	for _, r := range auth.Requirements {
		if r == req {
			return true
		}
	}
	return false
}

func (d *Daemon) opConnectApprovalsList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	list := d.connectMgr.Queue().List()
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{
			"id": a.ID, "appPublicKey": a.AppPublicKey, "appId": a.AppID,
			"method": a.Method, "params": a.Params, "origin": a.Origin,
			"requirements": a.Requirements, "status": a.Status, "createdAt": a.CreatedAt,
		})
	}
	return map[string]any{"approvals": out}, nil
}

// opConnectBindingsList 返回当前有效的应用连接视图（由有效授权派生）。
// 后台不维护独立的 binding 注册表：授权即连接，撤销即断开。
func (d *Daemon) opConnectBindingsList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	gen := d.connectMgr.Generation()
	out := make([]map[string]any, 0)
	for _, a := range d.connectMgr.AuthStore().List() {
		if a.Generation == "" || a.Generation != gen {
			continue
		}
		out = append(out, map[string]any{
			"id": a.AuthorizationID, "appPublicKey": a.AppPublicKey,
			"origin": a.Origin, "network": a.Network, "generation": a.Generation,
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
			"appPublicKey": a.AppPublicKey, "appId": a.AppID, "appName": a.AppName,
			"network": a.Network, "origin": a.Origin, "generation": a.Generation,
			"createdAt": a.CreatedAt,
		})
	}
	return map[string]any{"authorizations": out}, nil
}

func (d *Daemon) opConnectAuthRevoke(ctx context.Context, p kmapi.ConnectAuthRevokeRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if strings.TrimSpace(p.AppPublicKey) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "appPublicKey 必填")
	}
	if err := d.connectMgr.AuthStore().Revoke(p.AppPublicKey); err != nil {
		return nil, errDetail(kmapi.ErrNotFound, "授权不存在")
	}
	return kmapi.Ok{Ok: true}, nil
}

// opConnectApprovalsDecide 只做终态转换，不执行原请求。
// 批准后的执行由 connect.request.exec（approvalId 轮询）认领一次并返回结果，
// 避免决定响应塞不下二进制附件，也避免重复执行。
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
	approval, first, err := d.connectMgr.Queue().Decide(p.ApprovalID, p.Decision)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	if !first {
		// 重复决定不重复执行。
		return map[string]any{"ok": true, "status": approval.Status, "deduped": true}, nil
	}
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
	if a.Generation == "" || a.Generation != d.connectMgr.Generation() {
		return connect.Auth{}, errDetail(kmapi.ErrNotFound, "授权已过期，请重新登录")
	}
	if d.store == nil {
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
	// 重复认领返回已保存的结果，不重复执行。
	if p.ApprovalID != nil && *p.ApprovalID != "" && p.Method == nil {
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
			// 失败也要缓存成明确终态，重复查询不能变成空成功。
			d.connectMgr.Queue().StoreError(a.ID, perr.Code, perr.Message)
			return nil, nil, perr
		}
		d.connectMgr.Queue().StoreResult(a.ID, res, binaries)
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

	// launch 需要一次性启动令牌与完整 appView 会话；CLI 宿主尚未实现，明确拒绝，
	// 不能走 login 审批后误签发普通授权。
	if method == "connect.launch" {
		return nil, nil, errDetail(kmapi.ErrForbidden, "connect.launch 尚未实现")
	}
	// 建立连接类方法：proof 必填，每次调用都需用户确认绑定。
	// 例外：Receiver 已把持有授权的 reconnect 心跳转成 resume；此时只复核
	// 既有授权，不重复弹审批，也不改变授权内容。
	if isEstablishingConnectMethod(method) {
		if method == "connect.resume" && p.AuthorizationID != nil && *p.AuthorizationID != "" && p.Proof == nil {
			if strings.TrimSpace(origin) == "" {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "origin 缺失")
			}
			return d.resumeConnect(*p.AuthorizationID, origin)
		}
		var proofRaw json.RawMessage
		if p.Proof != nil {
			b, _ := json.Marshal(p.Proof)
			proofRaw = b
		}
		if len(proofRaw) == 0 {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "proof 必填")
		}
		verified, err := connect.VerifyProof(proofRaw)
		if err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "App 身份无效: "+err.Error())
		}
		if strings.TrimSpace(origin) == "" {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "origin 缺失")
		}
		paramsMap := map[string]any{}
		if p.Params != nil {
			b, _ := json.Marshal(p.Params)
			_ = json.Unmarshal(b, &paramsMap)
		}
		a := d.connectMgr.Queue().Create(
			verified.PublisherPublicKey, verified.AppID, verified.AppName,
			method, paramsMap, origin, verified.Requirements, verified.IdentityDigestHex,
		)
		return map[string]any{"approvalId": a.ID, "status": "pending"}, nil, nil
	}

	// 登出：需有效授权，自动执行（撤销），不经审批队列。
	if method == "connect.logout" {
		var authorizationID string
		if p.AuthorizationID != nil {
			authorizationID = *p.AuthorizationID
		}
		a, perr := d.validateConnectAuth(authorizationID, origin)
		if perr != nil {
			return nil, nil, perr
		}
		_ = a
		if err := d.connectMgr.AuthStore().RevokeByID(authorizationID); err != nil {
			return nil, nil, errDetail(kmapi.ErrNotFound, "授权不存在")
		}
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
	req, supported := connectRequirement(method)
	if !supported {
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
	if req != "" && !hasRequirement(auth, req) {
		return nil, nil, errDetail(kmapi.ErrForbidden, "无 "+req+" 操作许可")
	}
	appCtx := WithAppCaller(ctx)
	switch {
	case method == "identity.get":
		return nil, nil, errDetail(kmapi.ErrInternal, "identity.get 尚未实现（需 CBOR 信封 v2）")
	case strings.HasPrefix(method, "storage."):
		return d.execConnectStorage(appCtx, auth, method, d.paramsMapOf(p), attachments)
	case method == "cipher.encrypt" || method == "cipher.decrypt":
		return d.execConnectCipher(appCtx, auth, method, d.paramsMapOf(p), attachments, origin)
	default:
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
}

// resumeConnect 复核既有授权并返回与登录同形的 resume 结果。
// 只由 Receiver 的 reconnect 心跳路径调用；授权、来源、网络、钱包、代际
// 任一不匹配都拒绝，不静默降级为新的审批或新的授权。
func (d *Daemon) resumeConnect(authorizationID, origin string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	auth, perr := d.validateConnectAuth(authorizationID, origin)
	if perr != nil {
		return nil, nil, perr
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	d.mu.Lock()
	walletPub := d.publicKey
	d.mu.Unlock()
	return map[string]any{
		"authorizationId": auth.AuthorizationID,
		"networkIdentity": map[string]any{
			"bsvNetwork": info.BsvNetwork, "publicKeyHex": walletPub,
		},
		"resolvedClaims": map[string]any{},
		"resolvedAt":     time.Now().UnixMilli(),
		"appIdentity": map[string]any{
			"version": 1, "publisherPublicKeyHex": auth.AppPublicKey,
			"appId": auth.AppID, "appName": auth.AppName, "identityDigestHex": auth.IdentityDigest,
		},
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
func (d *Daemon) execConnectMethod(ctx context.Context, a connect.Approval) (any, map[string][]byte, *kmapi.ErrorDetail) {
	switch {
	case a.Method == "connect.login" || a.Method == "connect.resume":
		if d.store == nil {
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
		// 同一 App 同一来源重新登录时替换旧授权，避免僵尸授权堆积。
		for _, old := range d.connectMgr.AuthStore().FindByApp(a.AppPublicKey) {
			if old.Origin == a.Origin {
				_ = d.connectMgr.AuthStore().RevokeByID(old.AuthorizationID)
			}
		}
		if err := d.connectMgr.AuthStore().Save(connect.Auth{
			AuthorizationID: authorizationID,
			AppPublicKey:    a.AppPublicKey, AppID: a.AppID, AppName: a.AppName,
			IdentityDigest: a.IdentityDigest, Requirements: append([]string(nil), a.Requirements...),
			Origin: a.Origin, Network: info.BsvNetwork, WalletPublicKey: walletPub,
			Generation: d.connectMgr.Generation(),
		}); err != nil {
			return nil, nil, errDetail(kmapi.ErrInternal, "保存授权失败")
		}
		return map[string]any{
			"authorizationId": authorizationID,
			"networkIdentity": map[string]any{
				"bsvNetwork": info.BsvNetwork, "publicKeyHex": walletPub,
			},
			"resolvedClaims": map[string]any{},
			"resolvedAt":     now,
			"appIdentity": map[string]any{
				"version": 1, "publisherPublicKeyHex": a.AppPublicKey,
				"appId": a.AppID, "appName": a.AppName, "identityDigestHex": a.IdentityDigest,
			},
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

// storageErrDetail 将存储层错误映射为协议错误，条件写冲突不伪装成内部错误。
func storageErrDetail(err error) *kmapi.ErrorDetail {
	switch {
	case errors.Is(err, appstorage.ErrConflict):
		return errDetail(kmapi.ErrConflict, "条件写冲突")
	case errors.Is(err, appstorage.ErrTooLarge):
		return errDetail(kmapi.ErrTooLarge, "对象过大")
	case errors.Is(err, appstorage.ErrInvalidPath):
		return errDetail(kmapi.ErrBadRequest, "路径非法")
	case errors.Is(err, appstorage.ErrInvalidCursor):
		return errDetail(kmapi.ErrBadRequest, "游标非法")
	case errors.Is(err, appstorage.ErrNotEmpty):
		return errDetail(kmapi.ErrConflict, "目录非空")
	default:
		return errDetail(kmapi.ErrInternal, err.Error())
	}
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

func isoMillis(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func (d *Daemon) execConnectStorage(ctx context.Context, auth connect.Auth, method string, params map[string]any, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.store == nil {
		return nil, nil, errDetail(kmapi.ErrNotInitialized, "尚未初始化")
	}
	if d.isStopping() {
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	d.mu.Lock()
	locked := d.state != StateUnlocked
	wallet := d.publicKey
	d.mu.Unlock()
	if locked {
		return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	network := info.BsvNetwork
	path := ""
	if method != "storage.list" {
		rawPath, _ := params["path"].(string)
		var perr error
		path, perr = normalizeStoragePath(rawPath)
		if perr != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
		}
	}
	switch method {
	case "storage.put", "storage.encrypted.put":
		if path == ".dir" || strings.HasSuffix(path, appstorage.DirectoryMarkerSuffix) {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
		}
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
		// 明文上限与 Connect SDK 一致；加密信封的十六进制体积由存储层上限
		// 单独覆盖，不能把信封大小误当成明文大小。
		if int64(len(content)) > kmapi.DefaultLimits().StoragePlainMaxBytes {
			return nil, nil, errDetail(kmapi.ErrTooLarge, "对象过大")
		}
		mime, _ := params["mime"].(string)
		if mime == "" {
			if mt, ok := params["contentType"].(string); ok {
				mime = mt
			}
		}
		// overwrite=false 走存储层唯一约束的条件创建，不先读后写。
		ifNone := ""
		if ow, present := params["overwrite"]; present && ow == false {
			ifNone = "*"
		}
		if method == "storage.encrypted.put" {
			d.mu.Lock()
			priv := append([]byte(nil), d.privateKey...)
			pub := d.publicKey
			d.mu.Unlock()
			if len(priv) == 0 {
				return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
			}
			scope, serr := connect.ScopeFor(network, wallet, auth.AppPublicKey, path)
			if serr != nil {
				for i := range priv {
					priv[i] = 0
				}
				return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
			}
			env, encMIME, serr := connect.Seal(priv, pub, scope, content, mime)
			for i := range priv {
				priv[i] = 0
			}
			if serr != nil {
				return nil, nil, errDetail(kmapi.ErrInternal, serr.Error())
			}
			rev, updatedAt, serr := d.storage.Put(network, wallet, auth.AppPublicKey, path, env, encMIME, ifNone, 0)
			if serr != nil {
				return nil, nil, storageErrDetail(serr)
			}
			return map[string]any{
				"path": path, "size": len(content),
				"revision": strconv.FormatInt(rev, 10), "updatedAt": updatedAt,
			}, nil, nil
		}
		rev, updatedAt, err := d.storage.Put(network, wallet, auth.AppPublicKey, path, content, mime, ifNone, 0)
		if err != nil {
			return nil, nil, storageErrDetail(err)
		}
		return map[string]any{
			"path": path, "size": len(content),
			"revision": strconv.FormatInt(rev, 10), "updatedAt": updatedAt,
		}, nil, nil
	case "storage.get", "storage.encrypted.get":
		var offset int64
		if raw, present := params["offset"]; present && raw != nil {
			n, ok := toInt64(raw)
			if !ok || n < 0 {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "offset 非法")
			}
			offset = n
		}
		length := int64(-1)
		if raw, present := params["length"]; present && raw != nil {
			n, ok := toInt64(raw)
			if !ok || n < 0 {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "length 非法")
			}
			length = n
		}
		if method == "storage.encrypted.get" {
			// 范围作用于解密后的明文；必须读取同一版本的完整信封，不能先截断密文。
			full, err := d.storage.Get(network, wallet, auth.AppPublicKey, path, 0, -1)
			if err != nil {
				switch {
				case err == appstorage.ErrNotFound:
					return nil, nil, errDetail(kmapi.ErrNotFound, "对象不存在")
				case strings.Contains(err.Error(), "路径非法"):
					return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
				default:
					return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
				}
			}
			if raw, present := params["ifMatch"]; present && raw != nil {
				if want, _ := raw.(string); want != "" && want != strconv.FormatInt(full.Revision, 10) {
					return nil, nil, errDetail(kmapi.ErrConflict, "版本已变化")
				}
			}
			d.mu.Lock()
			priv := append([]byte(nil), d.privateKey...)
			pub := d.publicKey
			d.mu.Unlock()
			if len(priv) == 0 {
				return nil, nil, errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
			}
			scope, serr := connect.ScopeFor(network, wallet, auth.AppPublicKey, path)
			if serr != nil {
				for i := range priv {
					priv[i] = 0
				}
				return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
			}
			plain, ctype, serr := connect.Open(priv, pub, scope, full.Content)
			for i := range priv {
				priv[i] = 0
			}
			if serr != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "加密对象无法打开")
			}
			total := int64(len(plain))
			if offset > total {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "范围越界")
			}
			end := total
			if length >= 0 && length < total-offset {
				end = offset + length
			}
			chunk := plain[offset:end]
			return map[string]any{
				"path": path, "contentType": ctype, "offset": offset,
				"totalSize": total, "eof": end >= total,
				"revision":     strconv.FormatInt(full.Revision, 10),
				"lastModified": isoMillis(full.Updated),
			}, map[string][]byte{"content": chunk}, nil
		}
		obj, err := d.storage.Get(network, wallet, auth.AppPublicKey, path, offset, length)
		if err != nil {
			switch {
			case err == appstorage.ErrNotFound:
				return nil, nil, errDetail(kmapi.ErrNotFound, "对象不存在")
			case strings.Contains(err.Error(), "范围越界"):
				return nil, nil, errDetail(kmapi.ErrBadRequest, "范围越界")
			case strings.Contains(err.Error(), "路径非法"):
				return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
			default:
				return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
			}
		}
		// ifMatch：期望版本标签不一致则冲突，不返回内容。
		if raw, present := params["ifMatch"]; present && raw != nil {
			if want, _ := raw.(string); want != "" && want != strconv.FormatInt(obj.Revision, 10) {
				return nil, nil, errDetail(kmapi.ErrConflict, "版本已变化")
			}
		}
		eof := offset+int64(len(obj.Content)) >= obj.Total
		return map[string]any{
			"path": path, "contentType": obj.MIME, "offset": offset,
			"totalSize": obj.Total, "eof": eof,
			"revision":     strconv.FormatInt(obj.Revision, 10),
			"lastModified": isoMillis(obj.Updated),
		}, map[string][]byte{"content": obj.Content}, nil
	case "storage.list":
		rawPrefix, _ := params["prefix"].(string)
		prefix, perr := normalizeStoragePrefix(rawPrefix)
		if perr != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "路径非法")
		}
		storagePrefix := ""
		if prefix != "" {
			storagePrefix = prefix + "/"
		}
		cursor := ""
		if raw, present := params["cursor"]; present && raw != nil {
			cursor, _ = raw.(string)
		}
		limit := 0
		if raw, present := params["limit"]; present && raw != nil {
			n, ok := toInt64(raw)
			if !ok || n < 0 {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "limit 非法")
			}
			limit = int(n)
		}
		entries, next, err := d.storage.List(network, wallet, auth.AppPublicKey, storagePrefix, cursor, limit)
		if err != nil {
			return nil, nil, storageErrDetail(err)
		}
		dirs, files := splitConnectList(storagePrefix, entries)
		res := map[string]any{
			"prefix": storagePrefix, "parentPrefix": parentPrefixOf(storagePrefix),
			"directories": dirs, "files": files,
		}
		if next != "" {
			res["nextCursor"] = next
		}
		return res, nil, nil
	case "storage.directory.create":
		overwrite, _ := params["overwrite"].(bool)
		if _, _, err := d.storage.CreateDirectory(network, wallet, auth.AppPublicKey, path, overwrite); err != nil {
			return nil, nil, storageErrDetail(err)
		}
		return map[string]any{"path": path, "created": true}, nil, nil
	case "storage.directory.delete":
		if err := d.storage.DeleteDirectory(network, wallet, auth.AppPublicKey, path); err != nil {
			return nil, nil, storageErrDetail(err)
		}
		return map[string]any{"path": path, "deleted": true}, nil, nil
	case "storage.delete":
		if err := d.storage.Delete(network, wallet, auth.AppPublicKey, path); err != nil {
			if err == appstorage.ErrNotFound {
				return nil, nil, errDetail(kmapi.ErrNotFound, "对象不存在")
			}
			return nil, nil, errDetail(kmapi.ErrInternal, err.Error())
		}
		return map[string]any{"path": path, "deleted": true, "updatedAt": time.Now().UnixMilli()}, nil, nil
	default:
		return nil, nil, errDetail(kmapi.ErrInternal, "未实现")
	}
}

// normalizeStoragePath 规范化 App 相对路径：允许一个前导/末尾斜杠，拒绝空段、
// . / ..、反斜杠和 NUL；返回不带前导/末尾斜杠的规范形式。
func normalizeStoragePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	if strings.ContainsAny(p, "\\\x00") {
		return "", fmt.Errorf("路径非法")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("路径非法")
		}
	}
	return strings.Join(strings.Split(p, "/"), "/"), nil
}

// normalizeStoragePrefix 与路径规范一致，但空前缀表示 App 根。
func normalizeStoragePrefix(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return "", nil
	}
	return normalizeStoragePath(p)
}

// splitConnectList 将扁平前缀列举折叠为 SDK 的 directories/files 两层视图。
func splitConnectList(prefix string, entries []appstorage.Entry) (dirs []map[string]any, files []map[string]any) {
	dirs = []map[string]any{}
	files = []map[string]any{}
	seen := map[string]bool{}
	for _, en := range entries {
		rest := strings.TrimPrefix(en.Path, prefix)
		if rest == "" || rest == ".dir" {
			// 当前目录自身的 marker 是实现细节，不出现在文件列表。
			continue
		}
		if i := strings.Index(rest, "/"); i > 0 {
			seg := rest[:i]
			if !seen[seg] {
				seen[seg] = true
				// cc 的目录 path 不带末尾斜杠（prefix 本身仍以 / 结尾）。
				dirPath := strings.TrimSuffix(prefix, "/")
				if dirPath != "" {
					dirPath += "/"
				}
				dirPath += seg
				dirs = append(dirs, map[string]any{"path": dirPath, "name": seg})
			}
			continue
		}
		if rest == "" || strings.Contains(rest, "/") {
			continue
		}
		files = append(files, map[string]any{
			"path": en.Path, "name": rest, "size": en.Size,
			"revision":     strconv.FormatInt(en.Revision, 10),
			"lastModified": isoMillis(en.Updated),
		})
	}
	return dirs, files
}

func parentPrefixOf(prefix string) string {
	p := strings.TrimSuffix(prefix, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
	}
	return ""
}

func (d *Daemon) execConnectCipher(ctx context.Context, auth connect.Auth, method string, params map[string]any, attachments map[string][]byte, origin string) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if d.store == nil {
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
	siteKey, err := connect.DeriveCipherKey(priv, pub, info.BsvNetwork, origin, auth.AppPublicKey)
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
