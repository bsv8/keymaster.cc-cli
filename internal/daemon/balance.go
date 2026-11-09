package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// queryBalance returns the wallet balance with explicit semantics:
func (d *Daemon) queryBalance(ctx context.Context) (*balanceSnapshot, *balanceSnapshot, error) {
	d.mu.Lock()
	hasStore := d.store != nil
	unlocked := d.state == StateUnlocked
	d.mu.Unlock()
	if !hasStore {
		return nil, nil, fmt.Errorf("尚未初始化")
	}
	info, settings, err := d.store.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	client := woc.New(wocBaseURL(info.BsvNetwork), settings.WOCAPIKey)
	address := d.address
	if address == "" {
		return nil, d.cachedBalance(), fmt.Errorf("地址不可用")
	}
	rows, err := client.ListUnspent(ctx, address)
	if err != nil {
		return nil, d.cachedBalance(), fmt.Errorf("查询余额失败: %w", err)
	}
	var total uint64
	for _, u := range rows {
		total += u.Value
	}
	available := total
	if unlocked {
		if avail, aerr := d.poolAvailable(ctx, total); aerr == nil {
			available = avail
		} else {
			return nil, d.cachedBalance(), fmt.Errorf("查询可用余额失败: %w", aerr)
		}
	}
	var allocated uint64
	if total >= available {
		allocated = total - available
	}
	snap := &balanceSnapshot{
		Total: total, Available: available, Allocated: allocated,
		QueryTime: time.Now().UTC().Format(time.RFC3339Nano),
	}
	d.balanceMu.Lock()
	d.balanceCache = snap
	d.balanceMu.Unlock()
	return snap, nil, nil
}

func (d *Daemon) cachedBalance() *balanceSnapshot {
	d.balanceMu.Lock()
	defer d.balanceMu.Unlock()
	return d.balanceCache
}

// poolAvailable reports the in-memory allocation pool availability: chain
// total minus outpoints currently locked by in-flight funding. It reads the
// shared UTXO manager, so concurrent downloads never double-allocate and the
// balance view matches the funding path. When locked there is no pool, so
// availability equals the chain total. Refresh/allocation failures are
// returned so the caller shows an error (or stale cache), never a zero.
func (d *Daemon) poolAvailable(ctx context.Context, total uint64) (uint64, error) {
	d.mu.Lock()
	privateKey := append([]byte(nil), d.privateKey...)
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked || len(privateKey) == 0 {
		return total, nil
	}
	defer clearBytes(privateKey)
	info, settings, err := d.store.Snapshot()
	if err != nil {
		return 0, err
	}
	client := woc.New(wocBaseURL(info.BsvNetwork), settings.WOCAPIKey)
	w, release, err := wallet.Shared(client, wallet.Options{
		Network: info.BsvNetwork, PrivateKey: privateKey,
		FeeRateSatoshisPerKb: settings.FeeRateSatoshisPerKb,
	})
	if err != nil {
		return 0, fmt.Errorf("打开钱包分配池失败: %w", err)
	}
	defer release()
	if err := w.Refresh(ctx); err != nil {
		return 0, fmt.Errorf("刷新链上集合失败: %w", err)
	}
	available, err := w.Available(ctx)
	if err != nil {
		return 0, fmt.Errorf("读取可用金额失败: %w", err)
	}
	return available, nil
}

func wocBaseURL(network string) string {
	if network == "main" {
		return "https://api.whatsonchain.com/v1/bsv/main"
	}
	return "https://api.whatsonchain.com/v1/bsv/test"
}
