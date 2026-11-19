package daemon

import (
	"context"
	"os"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }
func (d *Daemon) directoryMarketURL() string {
	if url := getenv("KEYMASTER_MARKET_URL"); url != "" {
		return url
	}
	return "https://market.bsv8.com"
}

// opConnectDirectoryList 只读本地已归属数据；不发请求、不触发市场刷新。
// 市场同步由后台首次同步与 /settings/market 手动刷新负责。
func (d *Daemon) opConnectDirectoryList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	view := d.connectMgr.Directory().View()
	// 已验证连接的展示资料随使用记录保存；只作展示，不构成可启动入口。
	// 用户手动删除的 App 不进入展示投影，新的已验证连接清除删除标记后恢复。
	deleted := d.connectMgr.Directory().DeletedKeys()
	deletedSet := map[string]bool{}
	for _, key := range deleted {
		deletedSet[key] = true
	}
	known := map[string]apppublication.AppDisplay{}
	for key, display := range d.connectMgr.Permissions().Displays() {
		if deletedSet[key] {
			continue
		}
		known[key] = display
	}
	view["knownApplications"] = known
	return view, nil
}

// opConnectDirectoryRemove 是用户手动删除应用：写入删除标记，阻止来源插件
// 的重放恢复。待验证项归 Market（connect.market.remove）。
func (d *Daemon) opConnectDirectoryRemove(ctx context.Context, p kmapi.ConnectDirectoryRemoveRequest) (any, *kmapi.ErrorDetail) {
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权删除目录")
	}
	if p.AppPublicKeyHex == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "App public key required")
	}
	if err := d.connectMgr.Directory().Remove(p.AppPublicKeyHex); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	d.publishConnect()
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opConnectDirectoryFavorite(ctx context.Context, p kmapi.ConnectDirectoryFavoriteRequest) (any, *kmapi.ErrorDetail) {
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权收藏目录")
	}
	if err := d.connectMgr.Directory().Favorite(p.AppPublicKeyHex, p.Favorite); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	d.publishConnect()
	return kmapi.Ok{Ok: true}, nil
}

func (d *Daemon) opConnectDirectorySelect(ctx context.Context, p kmapi.ConnectDirectorySelectRequest) (any, *kmapi.ErrorDetail) {
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权修改目录")
	}
	if (p.URL == nil) == (p.Indexseedhashhex == nil) {
		return nil, errDetail(kmapi.ErrBadRequest, "exactly one channel is required")
	}
	if err := d.connectMgr.Directory().Select(p.AppPublicKeyHex, connect.DirectorySelection{URL: stringOrEmpty(p.URL), Indexseedhashhex: stringOrEmpty(p.Indexseedhashhex)}); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	d.publishConnect()
	return kmapi.Ok{Ok: true}, nil
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
