// Buyer matured refund: after the refund lock height, the buyer recovers an
// unclosed pool with the SDK's pre-signed refund transaction. A negotiated
// close always wins: once a Kind 13 exists, no conflicting spend is built.
package bitfs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/protocol"

	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
)

// RefundPreparation is a persisted matured refund ready to broadcast.
type RefundPreparation struct {
	PoolID string
	TxID   string
	RawTx  []byte
}

// RefundCompletion reports one broadcast attempt.
type RefundCompletion struct {
	PoolID string
	TxID   string
	RawTx  []byte
}

// PrepareMaturedRefund builds the pre-signed refund for an unclosed pool,
// persists its exact bytes and binds it in the ledger. It refuses before the
// refund lock height and when a negotiated close already exists.
func (t *BuyerTask) PrepareMaturedRefund(ctx context.Context, poolID string) (*RefundPreparation, error) {
	if t.Wallet == nil || t.Ledger == nil || t.Store == nil || t.Facts == nil {
		return nil, errors.New("bitfs: wallet, ledger, store and facts are required")
	}
	state, err := LoadPoolState(t.Store, poolID)
	if err != nil {
		return nil, err
	}
	if exists, err := t.Store.Exists(poolPath(poolID, poolKind13Name)); err != nil {
		return nil, err
	} else if exists {
		return nil, errors.New("bitfs: pool already has a negotiated close response")
	}
	facts := t.Facts()
	lock := protocol.RefundLockTime(state.Meta.ExpiryLockTime)
	if facts.BlockHeight < protocol.BlockHeight(lock) {
		return nil, fmt.Errorf("bitfs: refund lock height %d not reached (current %d)", lock, facts.BlockHeight)
	}
	verified, err := buyer.BuildMaturedRefund(facts, state.Evidence)
	if err != nil {
		return nil, fmt.Errorf("bitfs: build matured refund: %w", err)
	}
	rawTx := append([]byte(nil), verified.RawTx()...)
	txid, err := parseTxID(rawTx)
	if err != nil {
		return nil, err
	}
	if _, err := t.Ledger.Prepare(ledger.KindRefund, txid, hex.EncodeToString(rawTx)); err != nil {
		return nil, err
	}
	if err := t.Ledger.SetPoolTransaction(poolID, ledger.KindRefund, txid); err != nil {
		return nil, err
	}
	record, ok := t.Ledger.Pools()[poolID]
	if !ok {
		return nil, fmt.Errorf("bitfs: unknown pool %s", poolID)
	}
	switch record.State {
	case ledger.PoolOpen:
		if err := t.Ledger.SetPoolState(poolID, ledger.PoolRecoveryPending); err != nil {
			return nil, err
		}
	case ledger.PoolRecoveryPending, ledger.PoolClosed:
	default:
		return nil, fmt.Errorf("bitfs: pool %s cannot refund from state %s", poolID, record.State)
	}
	return &RefundPreparation{PoolID: poolID, TxID: txid, RawTx: rawTx}, nil
}

// BroadcastMaturedRefund broadcasts the exact saved refund bytes. An unknown
// result stays recovery-pending with the evidence preserved for retry; a
// confirmed push closes the pool.
func (t *BuyerTask) BroadcastMaturedRefund(ctx context.Context, poolID string) (*RefundCompletion, error) {
	record, ok := t.Ledger.Pools()[poolID]
	if !ok {
		return nil, fmt.Errorf("bitfs: unknown pool %s", poolID)
	}
	if record.State == ledger.PoolClosed {
		tx, ok := t.Ledger.Transaction(record.RefundTxID)
		if !ok {
			return nil, fmt.Errorf("bitfs: closed pool %s has no refund transaction", poolID)
		}
		return &RefundCompletion{PoolID: poolID, TxID: tx.TxID}, nil
	}
	if record.State != ledger.PoolRecoveryPending || record.RefundTxID == "" {
		return nil, fmt.Errorf("bitfs: pool %s has no prepared refund", poolID)
	}
	tx, ok := t.Ledger.Transaction(record.RefundTxID)
	if !ok {
		return nil, fmt.Errorf("bitfs: refund transaction %s is not in the ledger", record.RefundTxID)
	}
	txid, err := t.Wallet.BroadcastRaw(ctx, "pool-refund", tx.RawHex)
	if err != nil {
		return nil, err
	}
	if err := t.Ledger.MarkTransition(txid, ledger.TxObserved, ""); err != nil {
		return nil, err
	}
	if err := t.Ledger.SetPoolState(poolID, ledger.PoolClosed); err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(tx.RawHex)
	if err != nil {
		return nil, fmt.Errorf("bitfs: refund bytes are corrupt: %w", err)
	}
	return &RefundCompletion{PoolID: poolID, TxID: txid, RawTx: raw}, nil
}
