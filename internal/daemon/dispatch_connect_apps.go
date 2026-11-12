package daemon

import (
	"context"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

func (d *Daemon) opConnectPermissionsList(ctx context.Context, p kmapi.ConnectPermissionsListRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权查询许可")
	}
	var out []map[string]any
	for _, e := range d.connectMgr.Permissions().Snapshot() {
		if p.ApplicationKey != nil && *p.ApplicationKey != "" && e.AppKey != *p.ApplicationKey {
			continue
		}
		out = append(out, map[string]any{
			"applicationKey": e.AppKey, "origin": e.Origin, "appId": e.AppID, "appPublicKey": e.AppPublicKey,
			"methods": append([]string(nil), e.Methods...), "updatedAt": e.UpdatedAt,
		})
	}
	if out == nil {
		out = []map[string]any{}
	}
	return map[string]any{"permissions": out}, nil
}

func (d *Daemon) opConnectPermissionsRevoke(ctx context.Context, p kmapi.ConnectPermissionsRevokeRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权撤销许可")
	}
	if strings.TrimSpace(p.ApplicationKey) == "" || strings.TrimSpace(p.Method) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "applicationKey/method 必填")
	}
	if !connect.IsApprovalMethod(p.Method) {
		return nil, errDetail(kmapi.ErrBadRequest, "操作不支持自动许可")
	}
	if perr := d.revokeAutoPermission(p.ApplicationKey, p.Method); perr != nil {
		return nil, perr
	}
	d.publishConnect()
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opConnectHistoryList(ctx context.Context, p kmapi.ConnectHistoryListRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权查询历史")
	}
	appKey := ""
	if p.ApplicationKey != nil {
		appKey = *p.ApplicationKey
	}
	limit := 200
	if p.Limit != nil && *p.Limit > 0 {
		limit = *p.Limit
		if limit > 1000 {
			limit = 1000
		}
	}
	events := d.connectMgr.History().List(appKey, limit)
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"id": e.ID, "at": e.At, "applicationKey": e.AppKey, "origin": e.Origin,
			"appId": e.AppID, "appPublicKey": e.AppPublicKey, "kind": e.Kind,
			"method": e.Method, "auto": e.Auto, "errorCode": e.ErrorCode, "detail": e.Detail,
		})
	}
	return map[string]any{"events": out}, nil
}

func (d *Daemon) opConnectAppsList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权查询应用")
	}
	// 应用身份合并授权、许可与历史三方来源：退出/断开删除授权后，有许可或历史
	// 的应用仍保留管理入口（撤销许可/查询历史），不因授权消失而无法选中。
	type appInfo struct {
		origin, appID, appName, pubkey, digest string
	}
	merged := map[string]*appInfo{}
	touch := func(key, origin, appID, appName, pubkey, digest string) *appInfo {
		e, ok := merged[key]
		if !ok {
			e = &appInfo{origin: origin, appID: appID}
			merged[key] = e
		}
		if e.origin == "" {
			e.origin = origin
		}
		if e.appID == "" {
			e.appID = appID
		}
		if appName != "" && e.appName == "" {
			e.appName = appName
		}
		if pubkey != "" && e.pubkey == "" {
			e.pubkey = pubkey
		}
		if digest != "" && e.digest == "" {
			e.digest = digest
		}
		return e
	}
	for _, a := range d.connectMgr.AuthStore().List() {
		key := connect.ApplicationKey(a.Origin, a.AppPublicKey, a.AppID)
		touch(key, a.Origin, a.AppID, a.AppName, a.AppPublicKey, a.IdentityDigest)
	}
	for _, e := range d.connectMgr.Permissions().Snapshot() {
		touch(e.AppKey, e.Origin, e.AppID, "", e.AppPublicKey, "")
	}
	for _, e := range d.connectMgr.History().List("", 1000) {
		if e.AppKey == "" {
			continue
		}
		touch(e.AppKey, e.Origin, e.AppID, "", e.AppPublicKey, "")
	}
	var apps []map[string]any
	for key, e := range merged {
		apps = append(apps, map[string]any{
			"applicationKey": key, "origin": e.origin, "appId": e.appID, "appName": e.appName,
			"appPublicKey": e.pubkey, "identityDigestHex": e.digest,
		})
	}
	// 会话为应用下的在线连接实例（未关闭且未暂停），含创建时间与暂停标记。
	byID := map[string]connect.Auth{}
	for _, a := range d.connectMgr.AuthStore().List() {
		byID[a.AuthorizationID] = a
	}
	gen := d.connectMgr.Generation()
	connections := []map[string]any{}
	for _, c := range d.connectMgr.Connections().List() {
		if c.ClosedAt != 0 || c.PausedAt != 0 {
			continue
		}
		a, ok := byID[c.AuthID]
		if !ok || a.Generation == "" || a.Generation != gen {
			continue
		}
		key := connect.ApplicationKey(a.Origin, a.AppPublicKey, a.AppID)
		connections = append(connections, map[string]any{
			"id": c.ID, "authId": c.AuthID, "applicationKey": key,
			"origin": a.Origin, "network": a.Network, "generation": a.Generation,
			"createdAt": c.CreatedAt, "lastSeen": c.LastSeen,
		})
	}
	perms := map[string][]string{}
	for _, e := range d.connectMgr.Permissions().Snapshot() {
		perms[e.AppKey] = append([]string(nil), e.Methods...)
	}
	if apps == nil {
		apps = []map[string]any{}
	}
	if connections == nil {
		connections = []map[string]any{}
	}
	return map[string]any{
		"applications": apps, "connections": connections,
		"permissions": perms, "historyAvailable": true,
	}, nil
}

// opConnectConnectionPause 暂停运行连接（保留授权）。
// 钱包管理上下文专用（钱包页面 teardown 时尽力调用）；已暂停/已关闭幂等成功。
func (d *Daemon) opConnectConnectionPause(ctx context.Context, p kmapi.ConnectConnectionPauseRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权暂停连接")
	}
	if strings.TrimSpace(p.AuthorizationID) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "authorizationId 必填")
	}
	a, ok := d.connectMgr.AuthStore().FindByID(p.AuthorizationID)
	if !ok {
		return nil, errDetail(kmapi.ErrNotFound, "授权不存在或已失效")
	}
	_ = a
	if d.connectMgr.Connections() != nil {
		d.connectMgr.Connections().Pause(p.AuthorizationID)
	}
	d.publishConnect()
	return kmapi.Ok{Ok: true}, nil
}
