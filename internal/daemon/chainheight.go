package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// chainHeight is a mutex-guarded chain tip shared by long-lived loops so
// quotes and protocol checks never run on a launch-time value forever.
type chainHeight struct {
	mu    sync.RWMutex
	value uint32
}

func (h *chainHeight) get() uint32 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.value
}

func (h *chainHeight) set(value uint32) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.value = value
}

// refreshLoop re-reads the tip every minute until ctx ends. Failures keep
// the last known height; callers surface stale-height risk via logs, never
// by failing the whole loop.
func (h *chainHeight) refreshLoop(ctx context.Context, client *woc.Client) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if fetched, err := client.ChainHeight(ctx); err == nil {
				h.set(fetched)
			}
		}
	}
}
