package bitfs

import (
	"errors"
	"fmt"

	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
)

// Preserve the manager's conclusion in the host ledger. An HTTP conflict alone
// is not a terminal failure: only the manager's verified input conclusion is.
func recordBroadcastFailure(l *ledger.Ledger, txid string, broadcastErr error) error {
	state := ledger.TxResultUnknown
	var conflict *wallet.InputConflictError
	if errors.As(broadcastErr, &conflict) && conflict.Result != nil && conflict.Result.Delivery == "failed" {
		state = ledger.TxFailed
	}
	if err := l.MarkTransition(txid, state, broadcastErr.Error()); err != nil {
		return errors.Join(broadcastErr, fmt.Errorf("bitfs: record broadcast conclusion: %w", err))
	}
	return broadcastErr
}
