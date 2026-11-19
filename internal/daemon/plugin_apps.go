package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// appsAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) appsAPIPlugin() webloom.Plugin {
	return apiPlugin("apps", map[string]apiHandler{
		kmapi.MethodConnectLaunchIssue: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectLaunchIssueRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectLaunchIssue(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDirectoryList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectDirectoryList(ctx)
			return res, nil, perr
		},
		kmapi.MethodConnectDirectoryRemove: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectDirectoryRemoveRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectDirectoryRemove(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDirectoryFavorite: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectDirectoryFavoriteRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectDirectoryFavorite(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDirectorySelect: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectDirectorySelectRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, err.Error())
			}
			res, perr := d.opConnectDirectorySelect(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDeploymentInstall: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectDeploymentInstallRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, err.Error())
			}
			res, perr := d.opConnectDeploymentInstall(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDeploymentStart: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectDeploymentStartRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, err.Error())
			}
			res, perr := d.opConnectDeploymentStart(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDeploymentStop: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.ConnectDeploymentStopRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, err.Error())
			}
			res, perr := d.opConnectDeploymentStop(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectDeploymentList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectDeploymentList(ctx)
			return res, nil, perr
		},
	})
}
