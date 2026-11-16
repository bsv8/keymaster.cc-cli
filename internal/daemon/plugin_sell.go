package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// sellAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) sellAPIPlugin() webloom.Plugin {
	return apiPlugin("sell", map[string]apiHandler{
		kmapi.MethodRootsList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opRootsList(ctx)
			return res, nil, perr
		},
		kmapi.MethodRootsAdd: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.AddRootRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opRootsAdd(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodRootsRemove: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.RootRemoveRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opRootsRemove(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodRootsSetDefault: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.SetDefaultRootRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opRootsSetDefault(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodFilesList: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opFilesList(ctx)
			return res, nil, perr
		},
		kmapi.MethodFilesBrowse: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.BrowseRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opBrowse(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodCatalogRefresh: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.CatalogRefreshRequest
			if len(params) > 0 {
				if err := decodeStrict(params, &p); err != nil {
					return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
				}
			}
			res, perr := d.opCatalogRefresh(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodPricingSetDefaults: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.PricingDefaultsRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opPricingSetDefaults(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodPricingBatch: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.PricingBatchRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opPricingBatch(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodPricingSetFile: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.PricingFileRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opPricingSetFile(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodPricingClearFile: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.PricingClearRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opPricingClearFile(ctx, p)
			return res, nil, perr
		},
	})
}
