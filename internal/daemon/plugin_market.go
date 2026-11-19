package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// marketAPIPlugin 归档 Market 正式方法：市场列表与同步、市场配置管理、
// 手工来源管理。
func (d *Daemon) marketAPIPlugin() webloom.Plugin {
	return apiPlugin("market", map[string]apiHandler{
		kmapi.MethodConnectMarketState: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opConnectMarketState(ctx)
			return res, nil, perr
		},
		kmapi.MethodConnectMarketSync: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.MarketSyncRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectMarketSync(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectMarketConfigure: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.MarketConfigureRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectMarketConfigure(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectMarketDiscard: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.MarketDiscardRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectMarketDiscard(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectMarketAdd: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.MarketAddRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectMarketAdd(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectMarketRemove: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.MarketRemoveRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectMarketRemove(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodConnectMarketRetry: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.MarketRetryRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opConnectMarketRetry(ctx, p)
			return res, nil, perr
		},
	})
}
