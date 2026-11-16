package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// downloadAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) downloadAPIPlugin() webloom.Plugin {
	return apiPlugin("download", map[string]apiHandler{
		kmapi.MethodTasksList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opTasksList(ctx)
			return res, nil, perr
		},
		kmapi.MethodTasksCreate: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.CreateTaskRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opTasksCreate(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodTasksGet: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.TaskGetRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opTasksGet(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodTasksCancel: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.TaskCancelRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opTasksCancel(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodTasksSubscribe: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.TaskSubscribeRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opTasksGet(ctx, kmapi.TaskGetRequest{SeedHashHex: p.SeedHashHex})
			return res, nil, perr
		},
	})
}
