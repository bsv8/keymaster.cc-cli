package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

func wocBaseURL(network string) string {
	if network == "main" {
		return "https://api.whatsonchain.com/v1/bsv/main"
	}
	return "https://api.whatsonchain.com/v1/bsv/test"
}

// chainClient owns the single WoC client for this daemon and wallet network.
func (d *Daemon) chainClient(network, apiKey string) *woc.Client {
	d.chainMu.Lock()
	defer d.chainMu.Unlock()
	if d.chain == nil {
		d.chain = woc.New(wocBaseURL(network), apiKey)
	}
	return d.chain
}

func (d *Daemon) updateChainAPIKey(apiKey string) {
	d.chainMu.Lock()
	defer d.chainMu.Unlock()
	if d.chain != nil {
		d.chain.SetAPIKey(apiKey)
	}
}

func (d *Daemon) balanceState() map[string]any {
	d.mu.Lock()
	funding := d.fundingWallet
	unlocked := d.state == StateUnlocked
	d.mu.Unlock()
	if !unlocked || funding == nil {
		return map[string]any{"cached": false}
	}
	value, observedAt, known := funding.Balance()
	out := map[string]any{"cached": false}
	d.balanceMu.Lock()
	refreshErr := d.balanceRefreshErr
	d.balanceMu.Unlock()
	if known {
		out["availableSats"] = value
		if !observedAt.IsZero() {
			out["queryTime"] = observedAt.Format(time.RFC3339Nano)
		}
	}
	if refreshErr != nil {
		out["error"] = refreshErr.Error()
		out["cached"] = known
	}
	return out
}

func (d *Daemon) startBalanceWatcher() {
	d.mu.Lock()
	funding := d.fundingWallet
	d.mu.Unlock()
	if funding == nil {
		return
	}
	parent := d.LifecycleContext()
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		refresh := func(observe func(context.Context) error) {
			ctx, cancel := context.WithTimeout(parent, 30*time.Second)
			defer cancel()
			err := observe(ctx)
			d.balanceMu.Lock()
			d.balanceRefreshErr = err
			d.balanceMu.Unlock()
		}
		refresh(funding.EnsureObserved)
		d.publishBalance()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-parent.Done():
				return
			case <-ticker.C:
				refresh(funding.Refresh)
			case <-funding.FundsChanged():
			}
			d.publishBalance()
		}
	}()
}

func (d *Daemon) publishBalance() {
	if d.events == nil {
		return
	}
	state := d.balanceState()
	available, known := state["availableSats"].(uint64)
	current := lastBalance{Available: available, Known: known, Err: fmt.Sprint(state["error"]), Cached: state["cached"] == true, published: true}
	d.balanceMu.Lock()
	changed := current != d.lastPublished
	d.lastPublished = current
	d.balanceMu.Unlock()
	if changed {
		d.events.Publish(kmapi.TopicBalance, "", nil, state)
	}
}
