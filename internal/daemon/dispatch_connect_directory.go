package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func getenv(k string) string { return strings.TrimSpace(os.Getenv(k)) }
func (d *Daemon) directoryMarketURL() string {
	if url := getenv("KEYMASTER_MARKET_URL"); url != "" {
		return url
	}
	return "https://market.bsv8.com"
}

// fetchMarket 只接受公开 entries/urls/files 格式，拒绝旧 origin 目录及整份坏快照。
func fetchMarket(url string) ([]connect.DirectoryEntry, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimSuffix(url, "/") + "/v1/market")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("market status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("market snapshot too large")
	}
	var doc struct {
		Entries []connect.DirectoryEntry `json:"entries"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if err = connect.ValidateDirectorySnapshot(doc.Entries); err != nil {
		return nil, err
	}
	return doc.Entries, nil
}
func (d *Daemon) opConnectDirectoryList(ctx context.Context) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	store := d.connectMgr.Directory()
	if url := d.directoryMarketURL(); url != "" {
		leader, call := store.ClaimMarketFetch(time.Now().UnixMilli())
		if call != nil {
			if leader {
				entries, err := fetchMarket(url)
				_ = store.CompleteMarketFetch(entries, time.Now().UnixMilli(), err)
			} else {
				select {
				case <-call.Done():
				case <-ctx.Done():
					return nil, errDetail(kmapi.ErrInternal, ctx.Err().Error())
				}
			}
		}
	}
	return store.View(), nil
}
func (d *Daemon) opConnectDirectoryAdd(ctx context.Context, p kmapi.ConnectDirectoryAddRequest) (any, *kmapi.ErrorDetail) {
	if d.connectMgr == nil {
		return nil, errDetail(kmapi.ErrInternal, "Connect 未就绪")
	}
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权添加目录")
	}
	if (p.URL == nil) == (p.Indexseedhashhex == nil) {
		return nil, errDetail(kmapi.ErrBadRequest, "exactly one URL or seed is required")
	}
	store := d.connectMgr.Directory()
	pending, err := store.AddPending(connect.DirectoryPending{URL: stringOrEmpty(p.URL), Indexseedhashhex: stringOrEmpty(p.Indexseedhashhex), Name: stringOrEmpty(p.Name), Description: stringOrEmpty(p.Description), Icon: stringOrEmpty(p.Icon)})
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, err.Error())
	}
	if pending.Indexseedhashhex != "" {
		// install 在整包验证后统一解析 seed pending；此处不能重复提交目录。
		_, e := d.opConnectDeploymentInstall(ctx, kmapi.ConnectDeploymentInstallRequest{Indexseedhashhex: pending.Indexseedhashhex})
		if e != nil {
			if err := store.PendingError(pending.PendingID, fmt.Errorf("%s", e.Message)); err != nil {
				return nil, errDetail(kmapi.ErrInternal, err.Error())
			}
		}
	}
	d.publishConnect()
	return store.View(), nil
}
func (d *Daemon) opConnectDirectoryRemove(ctx context.Context, p kmapi.ConnectDirectoryRemoveRequest) (any, *kmapi.ErrorDetail) {
	if isAppCaller(ctx) {
		return nil, errDetail(kmapi.ErrOriginRejected, "App 无权删除目录")
	}
	if (p.AppPublicKeyHex == nil) == (p.PendingID == nil) {
		return nil, errDetail(kmapi.ErrBadRequest, "exactly one App public key or pending ID is required")
	}
	if err := d.connectMgr.Directory().Remove(stringOrEmpty(p.AppPublicKeyHex), stringOrEmpty(p.PendingID)); err != nil {
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
