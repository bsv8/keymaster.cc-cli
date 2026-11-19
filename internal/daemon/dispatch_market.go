package daemon

import (
	"context"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
)

func (d *Daemon) marketAccess(ctx context.Context) *kmapi.ErrorDetail {
	if isAppCaller(ctx) {
		return errDetail(kmapi.ErrOriginRejected, "App 无权管理市场来源")
	}
	if d.market == nil {
		return errDetail(kmapi.ErrInternal, "Market 未就绪")
	}
	return nil
}

func (d *Daemon) opConnectMarketState(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	if err := d.market.Ready(); err != nil {
		return nil, errDetail(kmapi.ErrInternal, err.Error())
	}
	return d.market.StateView(), nil
}

func (d *Daemon) opConnectMarketSync(ctx context.Context, p kmapi.MarketSyncRequest) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	var err error
	if p.MarketID != nil && *p.MarketID != "" {
		// 指定市场：单个市场在途共用同一任务，挂起不阻塞其他市场与手工操作。
		err = d.market.SyncMarket(ctx, *p.MarketID)
	} else {
		err = d.market.SyncAll(ctx)
	}
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	// 与首次同步共用同一次完成通知：其他已打开的 /apps 与市场设置页
	// 依赖 connect 事件重读目录，不只更新发起页的返回值。
	d.publishConnect()
	return d.market.StateView(), nil
}

func (d *Daemon) opConnectMarketConfigure(ctx context.Context, p kmapi.MarketConfigureRequest) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	if _, err := d.market.Configure(p.Name, p.DirectoryURL); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return d.market.StateView(), nil
}

func (d *Daemon) opConnectMarketDiscard(ctx context.Context, p kmapi.MarketDiscardRequest) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	if err := d.market.Discard(p.MarketID); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	d.publishConnect()
	return d.market.StateView(), nil
}

func (d *Daemon) opConnectMarketAdd(ctx context.Context, p kmapi.MarketAddRequest) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	url, seed := stringOrEmpty(p.URL), stringOrEmpty(p.Indexseedhashhex)
	if (url == "") == (seed == "") {
		return nil, errDetail(kmapi.ErrBadRequest, "exactly one URL or seed is required")
	}
	if _, err := d.market.Add(url, seed); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return d.market.StateView(), nil
}

func (d *Daemon) opConnectMarketRemove(ctx context.Context, p kmapi.MarketRemoveRequest) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	if err := d.market.Remove(p.PendingID); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	d.publishConnect()
	return d.market.StateView(), nil
}

func (d *Daemon) opConnectMarketRetry(ctx context.Context, p kmapi.MarketRetryRequest) (any, *kmapi.ErrorDetail) {
	if e := d.marketAccess(ctx); e != nil {
		return nil, e
	}
	if err := d.market.Retry(p.PendingID); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	return d.market.StateView(), nil
}
