// Buyer matured refund: after the refund lock height, the buyer recovers an
// unclosed pool with the SDK's pre-signed refund transaction. A negotiated
// close always wins: once a Kind 13 exists, no conflicting spend is built.
package bitfs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/protocol"

	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
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

// protocolDeclaredOutputs extracts the non-self outputs of an exact
// protocol transaction as the declared business outputs for the manager
// anchor. Outputs are SDK-determined; the anchor binds this exact shape.
func protocolDeclaredOutputs(walletScriptHex, rawHex string) ([]wallet.Output, error) {
	tx, err := transaction.NewTransactionFromHex(rawHex)
	if err != nil {
		return nil, fmt.Errorf("bitfs: parse protocol transaction: %w", err)
	}
	var declared []wallet.Output
	for _, out := range tx.Outputs {
		if out.LockingScript == nil {
			return nil, fmt.Errorf("bitfs: protocol output has no script")
		}
		if strings.EqualFold(out.LockingScript.String(), walletScriptHex) {
			continue
		}
		scriptCopy := *out.LockingScript
		declared = append(declared, wallet.Output{Satoshis: out.Satoshis, LockingScript: &scriptCopy})
	}
	return declared, nil
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
	// Explicit protocol submit: bind every refund input to exact known
	// source bytes (persisted funding + latest SDK-verified payment state).
	// Unbindable inputs fail closed instead of bypassing validation.
	state, err := LoadPoolState(t.Store, poolID)
	if err != nil {
		return nil, err
	}
	sources := make([][]byte, 0, 2)
	if fundingRaw, err := StoredFunding(t.Store, record.FundingTxID); err == nil {
		sources = append(sources, fundingRaw)
	} else {
		return nil, fmt.Errorf("bitfs: refund funding bytes unavailable: %w", err)
	}
	if latest := state.Evidence.LatestPaymentRawTx; len(latest) > 0 {
		sources = append(sources, latest)
	}
	proofs, err := wallet.BindInputs(tx.RawHex, sources)
	if err != nil {
		return nil, err
	}
	feeRate := uint64(state.Evidence.Opening.MinerFeeRateSatoshisPerKilobyte)
	if feeRate == 0 {
		feeRate = uint64(t.MinerFeeRate)
	}
	declared, err := protocolDeclaredOutputs(t.Wallet.LockingScript().String(), tx.RawHex)
	if err != nil {
		return nil, err
	}
	prepared, err := t.Wallet.PrepareProtocol(ctx, "pool-refund", wallet.ProtocolRequest{
		Business: "0", FeeRate: strconv.FormatUint(feeRate, 10), Declared: declared,
		SignedHex: tx.RawHex, Proofs: proofs,
		Binding: wallet.ProtocolBinding{Kind: "refund", PoolID: poolID},
	})
	if err != nil {
		return nil, err
	}
	txid, err := t.Wallet.BroadcastPrepared(ctx, "pool-refund", prepared)
	if err != nil {
		return nil, recordBroadcastFailure(t.Ledger, tx.TxID, err)
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
