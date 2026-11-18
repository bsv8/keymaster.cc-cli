package daemon

import (
	"context"

	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

const connectTransportKey ctxKey = "connect-transport"

// saveConnectAuth 持久化授权并绑定运行连接到实际钱包通道。持传输关闭锁
// 横跨保存与绑定，关闭与保存交错时不留下已保存但未绑定的授权：
// 传输已关闭直接失败，不保存；保存与绑定之间关闭被锁挡住，清理侧随后
// 通过 CloseOwner 终结刚绑定的连接。锁顺序统一为 closeMu → authMu →
// {authstore, connections}，任何路径不得逆序持有。
func (d *Daemon) saveConnectAuth(ctx context.Context, auth connect.Auth) error {
	transport, _ := ctx.Value(connectTransportKey).(*wsConn)
	if transport == nil {
		var saveErr error
		d.connectMgr.SerializedAuthChange(func() {
			saveErr = d.connectMgr.AuthStore().Save(auth)
			if saveErr == nil && d.connectMgr.Connections() != nil {
				d.connectMgr.Connections().UpsertActive(auth, "")
			}
		})
		return saveErr
	}
	transport.closeMu.Lock()
	defer transport.closeMu.Unlock()
	if transport.closed {
		return context.Canceled
	}
	var saveErr error
	d.connectMgr.SerializedAuthChange(func() {
		saveErr = d.connectMgr.AuthStore().Save(auth)
		if saveErr == nil && d.connectMgr.Connections() != nil {
			d.connectMgr.Connections().UpsertActive(auth, transport.connID)
		}
	})
	return saveErr
}
