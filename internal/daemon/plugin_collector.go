package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// collectorAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) collectorAPIPlugin() webloom.Plugin {
	return apiPlugin("collector", map[string]apiHandler{
		kmapi.MethodCollectorStatus: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opCollectorStatus(ctx)
			return res, nil, perr
		},
		kmapi.MethodCollectorHistory: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opCollectorHistory(ctx)
			return res, nil, perr
		},
		kmapi.MethodCollectorDescribe: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.CollectorDescriptionRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opCollectorDescribe(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodCollectorConfig: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.CollectorConfigRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opCollectorConfig(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodCollectorFlush: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opCollectorFlush(ctx)
			return res, nil, perr
		},
		kmapi.MethodCollectorRetry: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opCollectorRetry(ctx)
			return res, nil, perr
		},
	})
}
