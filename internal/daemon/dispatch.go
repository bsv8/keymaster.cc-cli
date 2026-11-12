package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
)

// 调用上下文：钱包管理调用与 App 调用必须不同。App 上下文禁止进入钱包管理方法。
type ctxKey string

const callerKey ctxKey = "km-caller"

const (
	callerWallet = "wallet"
	callerApp    = "app"
)

// WithWalletCaller 标记钱包管理调用。
func WithWalletCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerKey, callerWallet)
}

// WithAppCaller 标记 App 调用（Connect 经此进入）。
func WithAppCaller(ctx context.Context) context.Context {
	return context.WithValue(ctx, callerKey, callerApp)
}

func isAppCaller(ctx context.Context) bool {
	v, _ := ctx.Value(callerKey).(string)
	return v == callerApp
}

// walletMethods 为钱包管理方法集合；App 上下文一律拒绝，仅 connect.request.exec 例外。
func isWalletMethod(method string) bool {
	if method == kmapi.MethodConnectRequestExec {
		return false
	}
	return true
}

func errDetail(code, msg string) *kmapi.ErrorDetail {
	return &kmapi.ErrorDetail{Code: code, Message: msg}
}

func decodeStrict(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func (d *Daemon) needInit() *kmapi.ErrorDetail {
	if d.store == nil {
		return errDetail(kmapi.ErrNotInitialized, "尚未初始化，请先运行 km init")
	}
	return nil
}

func (d *Daemon) needUnlocked() *kmapi.ErrorDetail {
	d.mu.Lock()
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked {
		return errDetail(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	return nil
}

func (d *Daemon) needRunning() *kmapi.ErrorDetail {
	if d.isStopping() {
		return errDetail(kmapi.ErrStopping, "后台停止中")
	}
	return nil
}

// Dispatch 为统一后台分派器：WS 与进程内调用同一入口、同一校验、同一业务实现。
func (d *Daemon) Dispatch(ctx context.Context, method string, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 关闭期间停止接收：在途统计 Add 必须与停止标记检查原子进行，
	// 否则新请求可能在排空等待开始后溜入并在锁释放后写盘。
	d.dispatchMu.Lock()
	if d.dispatchStopping {
		d.dispatchMu.Unlock()
		return nil, nil, errDetail(kmapi.ErrStopping, "后台停止中")
	}
	d.dispatchWg.Add(1)
	d.dispatchMu.Unlock()
	defer d.dispatchWg.Done()
	// App 上下文禁止进入钱包管理方法。
	if isAppCaller(ctx) && isWalletMethod(method) {
		return nil, nil, errDetail(kmapi.ErrOriginRejected, "App 无权调用钱包管理方法")
	}
	if err := kmapi.ValidateMethod(method); err != nil {
		if per, ok := err.(*kmapi.ProtocolError); ok {
			return nil, nil, &kmapi.ErrorDetail{Code: per.Code, Message: per.Message}
		}
		return nil, nil, errDetail(kmapi.ErrBadRequest, "未知方法")
	}
	_ = attachments
	switch method {
	case kmapi.MethodStatusGet:
		res, perr := d.opStatus(ctx)
	return res, nil, perr
	case kmapi.MethodInfoGet:
		res, perr := d.opInfo(ctx)
	return res, nil, perr
	case kmapi.MethodWalletUnlock:
		var p kmapi.UnlockRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opUnlock(ctx, p)
	return res, nil, perr
	case kmapi.MethodWalletCreate:
		var p kmapi.WalletCreateRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opWalletCreate(ctx, p)
	return res, nil, perr
	case kmapi.MethodWalletImport:
		var p kmapi.WalletImportRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opWalletImport(ctx, p)
	return res, nil, perr
	case kmapi.MethodRootsList:
		res, perr := d.opRootsList(ctx)
	return res, nil, perr
	case kmapi.MethodRootsAdd:
		var p kmapi.AddRootRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opRootsAdd(ctx, p)
	return res, nil, perr
	case kmapi.MethodRootsRemove:
		var p kmapi.RootRemoveRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opRootsRemove(ctx, p)
	return res, nil, perr
	case kmapi.MethodRootsSetDefault:
		var p kmapi.SetDefaultRootRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opRootsSetDefault(ctx, p)
	return res, nil, perr
	case kmapi.MethodFilesList:
		res, perr := d.opFilesList(ctx)
	return res, nil, perr
	case kmapi.MethodFilesBrowse:
		var p kmapi.BrowseRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opBrowse(ctx, p)
	return res, nil, perr
	case kmapi.MethodCatalogRefresh:
		var p kmapi.CatalogRefreshRequest
		if len(params) > 0 {
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
		}
		res, perr := d.opCatalogRefresh(ctx, p)
	return res, nil, perr
	case kmapi.MethodSettingsGet:
		res, perr := d.opSettingsGet(ctx)
	return res, nil, perr
	case kmapi.MethodSettingsUpdate:
		var p kmapi.SettingsUpdate
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opSettingsUpdate(ctx, p)
	return res, nil, perr
	case kmapi.MethodSettingsReset:
		var p kmapi.RevisionRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opSettingsReset(ctx, p)
	return res, nil, perr
	case kmapi.MethodCollectorStatus:
		res, perr := d.opCollectorStatus(ctx)
	return res, nil, perr
	case kmapi.MethodCollectorHistory:
		res, perr := d.opCollectorHistory(ctx)
	return res, nil, perr
	case kmapi.MethodCollectorDescribe:
		var p kmapi.CollectorDescriptionRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opCollectorDescribe(ctx, p)
	return res, nil, perr
	case kmapi.MethodCollectorConfig:
		var p kmapi.CollectorConfigRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opCollectorConfig(ctx, p)
	return res, nil, perr
	case kmapi.MethodCollectorFlush:
		res, perr := d.opCollectorFlush(ctx)
	return res, nil, perr
	case kmapi.MethodCollectorRetry:
		res, perr := d.opCollectorRetry(ctx)
	return res, nil, perr
	case kmapi.MethodPricingSetDefaults:
		var p kmapi.PricingDefaultsRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opPricingSetDefaults(ctx, p)
	return res, nil, perr
	case kmapi.MethodPricingBatch:
		var p kmapi.PricingBatchRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opPricingBatch(ctx, p)
	return res, nil, perr
	case kmapi.MethodPricingSetFile:
		var p kmapi.PricingFileRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opPricingSetFile(ctx, p)
	return res, nil, perr
	case kmapi.MethodPricingClearFile:
		var p kmapi.PricingClearRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opPricingClearFile(ctx, p)
	return res, nil, perr
	case kmapi.MethodTasksList:
		res, perr := d.opTasksList(ctx)
	return res, nil, perr
	case kmapi.MethodTasksCreate:
		var p kmapi.CreateTaskRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opTasksCreate(ctx, p)
	return res, nil, perr
	case kmapi.MethodTasksGet:
		var p kmapi.TaskGetRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opTasksGet(ctx, p)
	return res, nil, perr
	case kmapi.MethodTasksCancel:
		var p kmapi.TaskCancelRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opTasksCancel(ctx, p)
	return res, nil, perr
	case kmapi.MethodTasksSubscribe:
		var p kmapi.TaskSubscribeRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opTasksGet(ctx, kmapi.TaskGetRequest{SeedHashHex: p.SeedHashHex})
	return res, nil, perr
	case kmapi.MethodEventsSubscribe:
		var p kmapi.EventsSubscribeRequest
		if len(params) > 0 {
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
		}
		res, perr := d.opEventsSubscribe(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectApprovalsList:
		res, perr := d.opConnectApprovalsList(ctx)
	return res, nil, perr
	case kmapi.MethodConnectBindingsList:
		res, perr := d.opConnectBindingsList(ctx)
	return res, nil, perr
	case kmapi.MethodConnectAuthList:
		res, perr := d.opConnectAuthList(ctx)
	return res, nil, perr
	case kmapi.MethodConnectAuthRevoke:
		var p kmapi.ConnectAuthRevokeRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectAuthRevoke(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectApprovalsDecide:
		var p kmapi.ConnectApprovalDecideRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectApprovalsDecide(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectRequestExec:
		return d.opConnectExec(ctx, params, attachments)
	case kmapi.MethodConnectLaunchIssue:
		var p kmapi.ConnectLaunchIssueRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectLaunchIssue(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectPermissionsList:
		var p kmapi.ConnectPermissionsListRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectPermissionsList(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectPermissionsRevoke:
		var p kmapi.ConnectPermissionsRevokeRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectPermissionsRevoke(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectHistoryList:
		var p kmapi.ConnectHistoryListRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectHistoryList(ctx, p)
	return res, nil, perr
	case kmapi.MethodConnectAppsList:
		res, perr := d.opConnectAppsList(ctx)
	return res, nil, perr
	case kmapi.MethodConnectConnectionPause:
		var p kmapi.ConnectConnectionPauseRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opConnectConnectionPause(ctx, p)
	return res, nil, perr
	case kmapi.MethodStoragePut:
		var p kmapi.StoragePutRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		return d.opStoragePut(ctx, p, attachments)
	case kmapi.MethodStorageGet:
		var p kmapi.StorageGetRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		return d.opStorageGet(ctx, p)
	case kmapi.MethodStorageList:
		var p kmapi.StorageListRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opStorageList(ctx, p)
	return res, nil, perr
	case kmapi.MethodStorageDelete:
		var p kmapi.StorageDeleteRequest
		if err := decodeStrict(params, &p); err != nil {
			return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
		}
		res, perr := d.opStorageDelete(ctx, p)
	return res, nil, perr
	default:
		return nil, nil, errDetail(kmapi.ErrBadRequest, "未知方法")
	}
}

func (d *Daemon) opStatus(ctx context.Context) (any, *kmapi.ErrorDetail) {
	d.mu.Lock()
	state := d.state
	hasStore := d.store != nil
	publicKey := d.publicKey
	address := d.address
	d.mu.Unlock()
	if !hasStore {
		return kmapi.Status{
			Initialized: false, State: string(StateUninitialized),
			Product: DaemonProduct, APIVersion: DaemonAPIVersion,
		}, nil
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return kmapi.Status{
		Initialized: true,
		State:       string(state),
		Network:     reqStr(info.BsvNetwork),
		Locked:      optBool(state != StateUnlocked, true),
		Address:     reqStr(address),
		PublicKey:   reqStr(publicKey),
		Product:     DaemonProduct,
		APIVersion:  DaemonAPIVersion,
	}, nil
}

func (d *Daemon) opInfo(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	d.mu.Lock()
	state := d.state
	address := d.address
	d.mu.Unlock()
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	out := kmapi.Info{
		Connected: true,
		Network:   info.BsvNetwork,
		Locked:    state != StateUnlocked,
		Address:   address,
	}
	bal, cached, queryErr := d.queryBalance(ctx)
	if queryErr != nil {
		out.Error = optStr(queryErr.Error())
		if cached != nil {
			out.TotalSats = optU64(cached.Total, true)
			out.AvailableSats = optU64(cached.Available, true)
			out.AllocatedSats = optU64(cached.Allocated, true)
			out.QueryTime = reqStr(cached.QueryTime)
			out.Cached = optBool(true, true)
		} else {
			out.Cached = optBool(false, true)
		}
	} else {
		out.TotalSats = optU64(bal.Total, true)
		out.AvailableSats = optU64(bal.Available, true)
		out.AllocatedSats = optU64(bal.Allocated, true)
		out.QueryTime = reqStr(bal.QueryTime)
		out.Cached = optBool(false, true)
	}
	return out, nil
}

func (d *Daemon) opUnlock(ctx context.Context, p kmapi.UnlockRequest) (any, *kmapi.ErrorDetail) {
	if strings.TrimSpace(p.Password) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "password is required")
	}
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if err := d.Unlock(p.Password); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "密码错误") || strings.Contains(msg, "解锁失败") {
			return nil, errDetail(kmapi.ErrBadRequest, "解锁失败：密码错误")
		}
		if strings.Contains(msg, "尚未初始化") {
			return nil, errDetail(kmapi.ErrNotInitialized, msg)
		}
		return nil, errDetail(kmapi.ErrInternal, msg)
	}
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opRootsList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	views := make([]kmapi.RootView, 0, len(info.Roots))
	for _, root := range info.Roots {
		accessible, errStr := true, ""
		if h, ok := d.roots.get(root.ID); ok {
			if h.err != "" {
				accessible, errStr = false, h.err
			} else if h.catalog == nil {
				accessible, errStr = false, "目录不可访问"
			}
		}
		views = append(views, kmapi.RootView{ID: root.ID, Path: root.Path, Accessible: accessible, Error: optStr(errStr)})
	}
	return kmapi.RootsView{
		Roots: views, DefaultDownloadRootID: reqStr(info.DefaultDownloadRootID),
		Revision: info.Revision,
	}, nil
}

func (d *Daemon) opRootsAdd(ctx context.Context, p kmapi.AddRootRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if e := d.needUnlocked(); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.Path) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "path is required")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	cleaned, err := appconfig.ValidateNewRootProtected(d.layout.Dir, []string{d.storageLayout.Dir}, p.Path, info.Roots)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	if d.roots.tryReapDeletedPath(cleaned) {
		return nil, errDetail(kmapi.ErrConflict, "该目录的旧售卖根仍有在途会话，待收尾后重试")
	}
	id, err := appconfig.NewRootID()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	updated, err := d.store.UpdateInfo(p.Revision, func(next *appconfig.Info) error {
		next.Roots = append(next.Roots, appconfig.Root{ID: id, Path: cleaned})
		if next.DefaultDownloadRootID == "" {
			next.DefaultDownloadRootID = id
		}
		return nil
	})
	if err != nil {
		return nil, errDetail(kmapi.ErrConflict, err.Error())
	}
	_, settings, _ := d.store.Snapshot()
	d.roots.tryReapDeletedPath(cleaned)
	d.roots.mu.Lock()
	d.roots.roots[id] = d.roots.openOne(ctx, appconfig.Root{ID: id, Path: cleaned}, settings.Server.DefaultSeedPriceSatoshis, settings.Server.DefaultFullBlockPriceSatoshis)
	d.roots.mu.Unlock()
	d.feedCollectors()
	d.publishRoots(id, updated.Revision)
	return kmapi.AddRootResponse{ID: id, Path: cleaned, Revision: updated.Revision}, nil
}

func (d *Daemon) opRootsRemove(ctx context.Context, p kmapi.RootRemoveRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if e := d.needUnlocked(); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "id is required")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	if info.DefaultDownloadRootID == p.ID {
		return nil, errDetail(kmapi.ErrBadRequest, "删除默认根前必须另选默认根")
	}
	if active := d.tasks.activeRoots(); active[p.ID] {
		return nil, errDetail(kmapi.ErrConflict, "该根有活动任务，请先完成或取消相关任务")
	}
	updated, err := d.store.UpdateInfo(p.Revision, func(next *appconfig.Info) error {
		kept := next.Roots[:0]
		found := false
		for _, root := range next.Roots {
			if root.ID == p.ID {
				found = true
				continue
			}
			kept = append(kept, root)
		}
		if !found {
			return fmt.Errorf("售卖根不存在")
		}
		next.Roots = append([]appconfig.Root(nil), kept...)
		return nil
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "售卖根不存在") {
			return nil, errDetail(kmapi.ErrNotFound, msg)
		}
		return nil, errDetail(kmapi.ErrConflict, msg)
	}
	d.roots.mu.Lock()
	if h, ok := d.roots.roots[p.ID]; ok {
		h.deleted = true
	}
	d.roots.mu.Unlock()
	d.roots.reapDeleted()
	d.publishRoots(p.ID, updated.Revision)
	return kmapi.RevisionedOk{Ok: true, Revision: updated.Revision}, nil
}

func (d *Daemon) opRootsSetDefault(ctx context.Context, p kmapi.SetDefaultRootRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if e := d.needUnlocked(); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.ID) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "id is required")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	found := false
	for _, r := range info.Roots {
		if r.ID == p.ID {
			found = true
			break
		}
	}
	if !found {
		return nil, errDetail(kmapi.ErrNotFound, "售卖根不存在")
	}
	updated, err := d.store.UpdateInfo(p.Revision, func(next *appconfig.Info) error {
		next.DefaultDownloadRootID = p.ID
		return nil
	})
	if err != nil {
		return nil, errDetail(kmapi.ErrConflict, err.Error())
	}
	d.publishRoots(p.ID, updated.Revision)
	return kmapi.RevisionedOk{Ok: true, Revision: updated.Revision}, nil
}

func (d *Daemon) opFilesList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	views := []kmapi.FileView{}
	for _, h := range d.roots.list() {
		if h.deleted || h.catalog == nil || h.pricing == nil {
			continue
		}
		entries, err := h.catalog.List(ctx)
		if err != nil {
			continue
		}
		for _, e := range entries {
			seedPrice, blockPrice, override := h.pricing.Effective(e.RelativePath)
			var blocks, size *uint64
			if e.State == "ready" {
				c := e.BlockCount
				blocks = &c
			}
			if e.Size > 0 {
				v := e.Size
				size = &v
			}
			views = append(views, kmapi.FileView{
				RootID: h.root.ID, Path: e.RelativePath, Name: e.Name,
				Size: size, SeedHash: e.SeedHashHex, Blocks: blocks,
				SeedPrice: seedPrice, BlockPrice: blockPrice, Override: override,
				State: e.State, Error: optStr(e.Error),
			})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].RootID != views[j].RootID {
			return views[i].RootID < views[j].RootID
		}
		return views[i].Path < views[j].Path
	})
	return kmapi.FilesView{Files: views}, nil
}

func (d *Daemon) opBrowse(ctx context.Context, p kmapi.BrowseRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	path := "/"
	if p.Path != nil && *p.Path != "" {
		path = *p.Path
	}
	dirs, parent, err := browseDirs(path)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return kmapi.BrowseView{Path: path, Parent: optStr(parent), Dirs: dirs}, nil
}

func (d *Daemon) opCatalogRefresh(ctx context.Context, p kmapi.CatalogRefreshRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	rootID := ""
	if p.RootID != nil {
		rootID = *p.RootID
	}
	force := false
	if p.Force != nil {
		force = *p.Force
	}
	out, err := d.refreshCatalog(ctx, rootID, force)
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return out, nil
}

func (d *Daemon) opSettingsGet(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	_, settings, err := d.store.Snapshot()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return settingsDTO(settings), nil
}

func (d *Daemon) opSettingsUpdate(ctx context.Context, p kmapi.SettingsUpdate) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	d.settingsApplyMu.Lock()
	updated, err := d.store.UpdateSettings(p.Revision, func(next *appconfig.Settings) error {
		return applySettingsUpdate(next, &p)
	})
	if err != nil {
		d.settingsApplyMu.Unlock()
		return nil, errDetail(kmapi.ErrConflict, err.Error())
	}
	for _, h := range d.roots.list() {
		if h.deleted || h.pricing == nil {
			continue
		}
		_ = h.pricing.SetDefaults(updated.Server.DefaultSeedPriceSatoshis, updated.Server.DefaultFullBlockPriceSatoshis)
	}
	d.rebuildCollectors()
	d.feedCollectors()
	d.settingsApplyMu.Unlock()
	d.publishSettings(updated.Revision)
	return settingsDTO(updated), nil
}

func (d *Daemon) opSettingsReset(ctx context.Context, p kmapi.RevisionRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	d.settingsApplyMu.Lock()
	updated, err := d.store.ResetSettings(p.Revision)
	if err != nil {
		d.settingsApplyMu.Unlock()
		return nil, errDetail(kmapi.ErrConflict, err.Error())
	}
	for _, h := range d.roots.list() {
		if h.deleted || h.pricing == nil {
			continue
		}
		_ = h.pricing.SetDefaults(updated.Server.DefaultSeedPriceSatoshis, updated.Server.DefaultFullBlockPriceSatoshis)
	}
	d.rebuildCollectors()
	d.feedCollectors()
	d.settingsApplyMu.Unlock()
	d.publishSettings(updated.Revision)
	return settingsDTO(updated), nil
}

func (d *Daemon) opCollectorStatus(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	return d.collectorStatusView(), nil
}

func (d *Daemon) opCollectorHistory(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	return d.collectorHistoryView(), nil
}

func (d *Daemon) opCollectorDescribe(ctx context.Context, p kmapi.CollectorDescriptionRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.SeedHash) == "" || strings.TrimSpace(p.Description) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "seedHash/description 必填")
	}
	if err := d.collectorSetDescription(ctx, p.SeedHash, p.Description); err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opCollectorConfig(ctx context.Context, p kmapi.CollectorConfigRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if err := d.collectorApplyConfig(ctx, p); err != nil {
		return nil, errDetail(kmapi.ErrConflict, err.Error())
	}
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opCollectorFlush(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	return d.collectorFlushView(ctx), nil
}

func (d *Daemon) opCollectorRetry(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	return d.collectorRetryView(ctx), nil
}

func (d *Daemon) opPricingSetDefaults(ctx context.Context, p kmapi.PricingDefaultsRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	updated, err := func() (appconfig.Settings, error) {
		d.settingsApplyMu.Lock()
		defer d.settingsApplyMu.Unlock()
		return d.store.SetDefaultsPrices(p.Revision, p.SeedPrice, p.BlockPrice)
	}()
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	d.settingsApplyMu.Lock()
	_, settings, _ := d.store.Snapshot()
	for _, h := range d.roots.list() {
		if h.deleted || h.pricing == nil {
			continue
		}
		_ = h.pricing.SetDefaults(settings.Server.DefaultSeedPriceSatoshis, settings.Server.DefaultFullBlockPriceSatoshis)
	}
	d.settingsApplyMu.Unlock()
	d.publishSettings(updated.Revision)
	return kmapi.PricingDefaultsResponse{SeedPrice: updated.Server.DefaultSeedPriceSatoshis, BlockPrice: updated.Server.DefaultFullBlockPriceSatoshis, Revision: updated.Revision}, nil
}

func (d *Daemon) opPricingBatch(ctx context.Context, p kmapi.PricingBatchRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.RootID) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "rootId is required")
	}
	h, ok := d.roots.get(p.RootID)
	if !ok || h.catalog == nil || h.pricing == nil {
		return nil, errDetail(kmapi.ErrNotFound, "售卖根不可用")
	}
	path := ""
	if p.Path != nil {
		path = *p.Path
	}
	entries, err := h.catalog.List(ctx)
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if path != "" && !(e.RelativePath == path || len(e.RelativePath) > len(path)+1 && e.RelativePath[:len(path)+1] == path+"/") {
			continue
		}
		paths = append(paths, e.RelativePath)
	}
	switch p.Mode {
	case "all":
		if err := h.pricing.BatchAll(paths, p.SeedPrice, p.BlockPrice); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	case "unpriced":
		if err := h.pricing.BatchUnpriced(paths, p.SeedPrice, p.BlockPrice); err != nil {
			return nil, errDetail(kmapi.ErrBadRequest, err.Error())
		}
	default:
		return nil, errDetail(kmapi.ErrBadRequest, "mode must be all or unpriced")
	}
	return kmapi.PricingBatchResponse{Updated: len(paths)}, nil
}

func (d *Daemon) opPricingSetFile(ctx context.Context, p kmapi.PricingFileRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	h, ok := d.roots.get(p.RootID)
	if !ok || h.pricing == nil {
		return nil, errDetail(kmapi.ErrNotFound, "售卖根不可用")
	}
	if err := h.pricing.SetFile(p.Path, p.SeedPrice, p.BlockPrice); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return kmapi.PricingFileResponse{Updated: p.Path}, nil
}

func (d *Daemon) opPricingClearFile(ctx context.Context, p kmapi.PricingClearRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if strings.TrimSpace(p.RootID) == "" || strings.TrimSpace(p.Path) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "rootId and path are required")
	}
	h, ok := d.roots.get(p.RootID)
	if !ok || h.pricing == nil {
		return nil, errDetail(kmapi.ErrNotFound, "售卖根不可用")
	}
	if err := h.pricing.ClearFile(p.Path); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return kmapi.PricingClearResponse{Cleared: p.Path}, nil
}

func (d *Daemon) opTasksList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	return taskListDTO(d.tasks.list()), nil
}

func (d *Daemon) opTasksCreate(ctx context.Context, p kmapi.CreateTaskRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if e := d.needUnlocked(); e != nil {
		return nil, e
	}
	seed := strings.ToLower(strings.TrimSpace(p.SeedHashHex))
	if !validSeedHash(seed) {
		return nil, errDetail(kmapi.ErrBadRequest, "seedHashHex 无效")
	}
	task, _, err := d.ensureTask(ctx, seed)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return taskDTO(task), nil
}

func (d *Daemon) opTasksGet(ctx context.Context, p kmapi.TaskGetRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	seed := strings.ToLower(strings.TrimSpace(p.SeedHashHex))
	if !validSeedHash(seed) {
		return nil, errDetail(kmapi.ErrBadRequest, "seedHashHex 无效")
	}
	t, ok := d.tasks.get(seed)
	if !ok || t == nil {
		t = d.loadPersistedTask(seed)
	}
	if t == nil {
		return nil, errDetail(kmapi.ErrNotFound, "任务不存在")
	}
	cp := *t
	return taskDTO(&cp), nil
}

func (d *Daemon) opTasksCancel(ctx context.Context, p kmapi.TaskCancelRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	seed := strings.ToLower(strings.TrimSpace(p.SeedHashHex))
	if !validSeedHash(seed) {
		return nil, errDetail(kmapi.ErrBadRequest, "seedHashHex 无效")
	}
	if err := d.cancelTask(seed); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opEventsSubscribe(ctx context.Context, p kmapi.EventsSubscribeRequest) (any, *kmapi.ErrorDetail) {
	if e := d.needInit(); e != nil {
		return nil, e
	}
	topics := p.Topics
	for _, t := range topics {
		if !isKnownTopic(t) {
			return nil, errDetail(kmapi.ErrBadRequest, "未知订阅主题")
		}
	}
	snap, err := d.buildSnapshot(topics)
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return snap, nil
}

var (
	_ = sort.StringSlice{}
	_ = fmt.Sprintf
)
