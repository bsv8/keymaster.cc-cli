package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// protocolAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) protocolAPIPlugin() webloom.Plugin {
	return apiPlugin("protocol", map[string]apiHandler{
		kmapi.MethodConnectApprovalsList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectApprovalsList(ctx)
			return res, nil, perr
		},
		kmapi.MethodConnectBindingsList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectBindingsList(ctx)
			return res, nil, perr
		},
		kmapi.MethodConnectAuthList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectAuthList(ctx)
			return res, nil, perr
		},
		kmapi.MethodConnectAuthRevoke: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectAuthRevokeRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectAuthRevoke(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectApprovalsDecide: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectApprovalDecideRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectApprovalsDecide(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectRequestExec: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			return d.opConnectExec(ctx, params, attachments)
		},
		kmapi.MethodConnectPermissionsList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectPermissionsListRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectPermissionsList(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectPermissionsRevoke: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectPermissionsRevokeRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectPermissionsRevoke(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectHistoryList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectHistoryListRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectHistoryList(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectAppsList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectAppsList(ctx)
			return res, nil, perr
		},
		kmapi.MethodConnectConnectionPause: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectConnectionPauseRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectConnectionPause(ctx, p)
			return res, nil, perr
		},
	})
}
