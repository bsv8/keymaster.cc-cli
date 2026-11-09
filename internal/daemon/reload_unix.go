//go:build unix

package daemon

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// watchReload starts the periodic catalog check (same ordinary check as
// startup and manual refresh) plus SIGHUP-triggered checks. It stops when
// ctx ends. Failures are recorded per root, never fatal.
func (d *Daemon) watchReload(ctx context.Context) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	d.refreshAndFeed(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			d.refreshAndFeed(ctx)
		case <-ticker.C:
			d.refreshAndFeed(ctx)
		}
	}
}

// refreshAndFeed re-checks all roots and queues new mappings to collectors.
func (d *Daemon) refreshAndFeed(ctx context.Context) {
	if d.roots == nil {
		return
	}
	d.roots.refreshAll(ctx)
	d.feedCollectors()
}
