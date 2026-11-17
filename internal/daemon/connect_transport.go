package daemon

import (
	"context"

	"github.com/bsv8/keymaster.cc-cli/internal/connect"
)

const connectTransportKey ctxKey = "connect-transport"

// registerConnectConnection 把新授权的运行连接归属到实际钱包通道。
// 与 cleanup 的 closed 标记共用锁，断开后迟到的登录结果不能留下在线连接。
func (d *Daemon) registerConnectConnection(ctx context.Context, auth connect.Auth) bool {
	transport, _ := ctx.Value(connectTransportKey).(*wsConn)
	if transport == nil {
		d.connectMgr.Connections().UpsertActive(auth, "")
		return true
	}
	transport.closeMu.Lock()
	defer transport.closeMu.Unlock()
	if transport.closed {
		return false
	}
	d.connectMgr.Connections().UpsertActive(auth, transport.connID)
	return true
}
