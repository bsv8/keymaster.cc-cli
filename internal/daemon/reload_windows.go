//go:build windows

package daemon

import (
	"context"
	"time"
)

// watchReload starts the periodic catalog check on Windows (no SIGHUP).
func (d *Daemon) watchReload(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	d.refreshAndFeed(ctx)
	for {
		select {
		case <-ctx.Done():
			return
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
