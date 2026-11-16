package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// backgroundAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) backgroundAPIPlugin() webloom.Plugin {
	return apiPlugin("background", map[string]apiHandler{
		kmapi.MethodSettingsGet: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opSettingsGet(ctx)
			return res, nil, perr
		},
		kmapi.MethodSettingsUpdate: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.SettingsUpdate
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opSettingsUpdate(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodSettingsReset: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.RevisionRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opSettingsReset(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodEventsSubscribe: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.EventsSubscribeRequest
			if len(params) > 0 {
				if err := decodeStrict(params, &p); err != nil {
					return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
				}
			}
			res, perr := d.opEventsSubscribe(ctx, p)
			return res, nil, perr
		},
	})
}
