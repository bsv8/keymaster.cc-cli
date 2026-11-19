package daemon

import (
	"context"
	"path/filepath"

	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"github.com/bsv8/keymaster.cc-cli/internal/market"
)

// initMarketManager 装配 Market 管理器：目录唯一写入方、使用判定、
// URL 验签与 Files 整包验证复用现有能力。后台任务绑定 daemon 生命周期，
// Close 时取消并等待退出后才释放数据目录锁。
func (d *Daemon) initMarketManager() error {
	var directoryURL string
	if base := getenv("KEYMASTER_MARKET_URL"); base != "" {
		var err error
		directoryURL, err = market.EntryDirectoryURL(base)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := market.NewManager(ctx, filepath.Join(d.layout.Dir, "apps"), market.Deps{
		BuiltInDirectoryURL: directoryURL,
		Dir:                 d.connectMgr.Directory(),
		HasUsage:            d.appHasUsage,
		FetchMarket:         market.FetchMarketEntries,
		VerifyURL:           market.VerifyManualURL,
		VerifySeed:          d.verifySeedPublication,
		OnChange:            d.publishConnect,
	})
	if err != nil {
		cancel()
		return err
	}
	d.market = mgr
	d.marketCancel = cancel

	return nil
}

// appHasUsage 判断 App 是否有使用数据（授权、许可或历史）；无法确认时
// 按“有使用”处理，来源失效后保留条目。
func (d *Daemon) appHasUsage(key string) bool {
	if d.connectMgr == nil {
		return true
	}
	for _, auth := range d.connectMgr.AuthStore().List() {
		if connect.ApplicationKey(auth.AppPublicKeyHex) == key {
			return true
		}
	}
	for _, perm := range d.connectMgr.Permissions().Snapshot() {
		if perm.AppKey == key {
			return true
		}
	}
	for _, event := range d.connectMgr.History().List("", 1000) {
		if event.AppKey == key {
			return true
		}
	}
	return false
}

// verifySeedPublication 复用部署安装完成 Files 整包验证；下载与验证
// 走现有任务调度与资金控制。
func (d *Daemon) verifySeedPublication(ctx context.Context, seed string) (market.SeedVerification, error) {
	result, err := d.deployments.Install(ctx, seed)
	if err != nil {
		return market.SeedVerification{}, err
	}
	return market.SeedVerification{
		AppPublicKeyHex: result.AppPublicKeyHex,
		AppVersion:      result.AppVersion,
		PublicationID:   result.PublicationID,
		Display:         result.Display,
	}, nil
}
