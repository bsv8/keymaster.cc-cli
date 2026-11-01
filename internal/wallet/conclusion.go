package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/woc"
	manager "github.com/bsv8/keymaster.cc-utxo-manager/utxo"
)

// InputConflictError is a stable original-request failure, not an instruction
// to Build another payment. The caller decides whether to create a new request.
type InputConflictError struct{ Result *manager.SubmitResult }

func (e *InputConflictError) Error() string {
	return fmt.Sprintf("wallet: input-conflict: original allocation %s failed; released %s sats", e.Result.AllocationID, e.Result.Conclusion.ReleasedSats)
}

type contextBroadcaster struct {
	ctx        context.Context
	chain      Chain
	diagnostic *string
}

func (b contextBroadcaster) Broadcast(raw string) (manager.BroadcastOutcome, error) {
	if err := b.ctx.Err(); err != nil {
		return manager.BroadcastOutcome{}, err
	}
	if adapter, ok := b.chain.(interface {
		BroadcastExact(context.Context, string) (woc.BroadcastReceipt, error)
	}); ok {
		receipt, err := adapter.BroadcastExact(b.ctx, raw)
		if b.diagnostic != nil {
			encoded, _ := json.Marshal(map[string]any{"receipt": receipt, "error": fmt.Sprint(err)})
			*b.diagnostic = string(encoded)
		}
		if err != nil {
			return manager.BroadcastOutcome{}, err
		}
		switch receipt.Status {
		case "accepted":
			return manager.BroadcastOutcome{Status: "accepted", TxID: receipt.TxID}, nil
		case "already-known":
			return manager.BroadcastOutcome{Status: "already-known", TxID: receipt.TxID, RawTxHex: raw}, nil
		default:
			return manager.BroadcastOutcome{}, &woc.BroadcastError{Receipt: receipt}
		}
	}
	txid, err := b.chain.Broadcast(b.ctx, raw)
	return manager.BroadcastOutcome{Status: "accepted", TxID: txid}, err
}

// PendingConclusions is read-only and never starts broadcasting or polling.
func (w *Wallet) PendingConclusions() ([]manager.OutboxEntry, error) {
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := w.checkLiveLocked(); err != nil {
		return nil, err
	}
	rows := []manager.OutboxEntry{}
	for _, row := range w.core.kernel.OutboxView() {
		if row.State != "done" && row.State != "failed" {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// QueryConclusionOnce explicitly dispatches the immutable original exact via
// the common Go/TS orchestration API. It never builds or signs a transaction.
func (w *Wallet) QueryConclusionOnce(ctx context.Context, allocationID string) (*manager.SubmitResult, error) {
	w.localMu.Lock()
	defer w.localMu.Unlock()
	w.core.mu.Lock()
	defer w.core.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := w.checkLiveLocked(); err != nil {
		return nil, err
	}
	diagnostic := ""
	return manager.RetryExact(w.core.kernel, manager.Ports{Store: managerStore{core: w.core, diagnostic: &diagnostic}, Broadcaster: contextBroadcaster{ctx: ctx, chain: w.chain, diagnostic: &diagnostic}, InputObserver: inputObserverFor(ctx, w.chain, &diagnostic)}, allocationID)
}

// QueryConclusion runs only after an explicit host invocation. No lock is held
// across backoff; Ctrl-C/Close stops future dispatch without releasing inputs.
func (w *Wallet) QueryConclusion(ctx context.Context, allocationID string, interval time.Duration) (*manager.SubmitResult, error) {
	if interval < time.Second {
		return nil, fmt.Errorf("wallet: conclusion interval must be at least one second")
	}
	w.localMu.Lock()
	if w.conclusionRunning == nil {
		w.conclusionRunning = map[string]bool{}
	}
	if w.conclusionRunning[allocationID] {
		w.localMu.Unlock()
		return nil, fmt.Errorf("wallet: conclusion query already running")
	}
	w.conclusionRunning[allocationID] = true
	w.localMu.Unlock()
	defer func() { w.localMu.Lock(); delete(w.conclusionRunning, allocationID); w.localMu.Unlock() }()
	for {
		result, err := w.QueryConclusionOnce(ctx, allocationID)
		if err != nil {
			return result, err
		}
		if result.Delivery != "unknown" {
			return result, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, ctx.Err()
		case <-timer.C:
		}
	}
}
