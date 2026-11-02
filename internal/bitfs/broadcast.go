package bitfs

import (
	"errors"
	"fmt"

	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// Submission results belong to the caller, not to the UTXO allocation pool.
func recordBroadcastFailure(l *ledger.Ledger, txid string, broadcastErr error) error {
	state := ledger.TxResultUnknown
	var rejection *woc.BroadcastError
	if errors.As(broadcastErr, &rejection) && rejection.Receipt.Status == "failed" {
		state = ledger.TxFailed
	}
	if err := l.MarkTransition(txid, state, broadcastErr.Error()); err != nil {
		return errors.Join(broadcastErr, fmt.Errorf("bitfs: record broadcast conclusion: %w", err))
	}
	return broadcastErr
}
