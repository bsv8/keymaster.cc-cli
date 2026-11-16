package daemon

import (
	"context"
	"encoding/json"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
)

// vaultAPIPlugin 把正式 API 方法归到业务单元；不改变原有业务校验与资金责任。
func (d *Daemon) vaultAPIPlugin() webloom.Plugin {
	return apiPlugin("vault", map[string]apiHandler{
		kmapi.MethodStatusGet: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opStatus(ctx)
			return res, nil, perr
		},
		kmapi.MethodInfoGet: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			res, perr := d.opInfo(ctx)
			return res, nil, perr
		},
		kmapi.MethodWalletUnlock: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.UnlockRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opUnlock(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodWalletCreate: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.WalletCreateRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opWalletCreate(ctx, p)
			return res, nil, perr
		},
		kmapi.MethodWalletImport: func(ctx context.Context, params json.RawMessage, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
			var p kmapi.WalletImportRequest
			if err := decodeStrict(params, &p); err != nil {
				return nil, nil, errDetail(kmapi.ErrBadRequest, "参数非法")
			}
			res, perr := d.opWalletImport(ctx, p)
			return res, nil, perr
		},
	})
}
