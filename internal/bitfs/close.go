// Buyer pool close: exact Kind 12 request bound to the newest local payment
// state, exact Kind 13 verification and observation of the seller-broadcast
// close transaction.
//
// The seller broadcasts the close transaction; the buyer never submits a
// second copy. It saves the same exact bytes, binds them in the ledger and
// then only reconciles by txid.
package bitfs

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/protocol"

	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// CloseChain is the read-only chain port used to observe the seller-broadcast
// close transaction. It may be nil: the pool then stays recovery-pending until
// ReconcileClose is called with a port available.
type CloseChain interface {
	TransactionRaw(ctx context.Context, txid string) ([]byte, error)
}

// CloseBinding pins the local Kind 5/7 state a close request was signed
// against, so a response can never be replayed onto another round.
type CloseBinding struct {
	Format               string `json:"format"`
	Version              int    `json:"version"`
	AuthorizationID      string `json:"authorizationId"`
	PaymentSequence      uint32 `json:"paymentSequence"`
	SellerAmountSatoshis uint64 `json:"sellerAmountSatoshis"`
}

// ClosePreparation is the persisted Kind 12 ready to send.
type ClosePreparation struct {
	PoolID                    string
	Kind12                    []byte
	AuthorizationID           string
	PaymentSequence           uint32
	SellerAmountAfterSatoshis uint64
}

// CloseCompletion is the verified close outcome. Closed reports that the
// seller-broadcast transaction was already observed on chain.
type CloseCompletion struct {
	TxID        string
	Kind13      []byte
	Transaction []byte
	Closed      bool
}

func closeBindingOf(state *PoolState) CloseBinding {
	return CloseBinding{
		Format:               "keymaster.bitfs-close-binding",
		Version:              1,
		AuthorizationID:      state.AuthorizationID,
		PaymentSequence:      state.PaymentSequence,
		SellerAmountSatoshis: state.SellerAmountSatoshis,
	}
}

func loadCloseBinding(s *store.Store, poolID string) (*CloseBinding, error) {
	raw, err := s.Load(poolPath(poolID, poolCloseBindingName))
	if err != nil {
		return nil, err
	}
	var binding CloseBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		return nil, fmt.Errorf("bitfs: decode close binding: %w", err)
	}
	if binding.Format != "keymaster.bitfs-close-binding" || binding.Version != 1 {
		return nil, errors.New("bitfs: close binding has an unsupported format")
	}
	return &binding, nil
}

// PrepareClose signs exact Kind 12 against the newest provable local payment
// state and persists it. The binding file is immutable: once a close has been
// requested, a different local state can never be silently substituted.
func (t *BuyerTask) PrepareClose(ctx context.Context, poolID string) (*ClosePreparation, error) {
	if t.Signer == nil || t.Facts == nil || t.Store == nil || t.Ledger == nil {
		return nil, errors.New("bitfs: store, ledger, signer and facts are required")
	}
	state, err := LoadPoolState(t.Store, poolID)
	if err != nil {
		return nil, err
	}
	poolRecord, ok := t.Ledger.Pools()[poolID]
	if !ok {
		return nil, fmt.Errorf("bitfs: unknown pool %s", poolID)
	}
	switch poolRecord.State {
	case ledger.PoolOpen, ledger.PoolRecoveryPending, ledger.PoolClosed:
	default:
		return nil, fmt.Errorf("bitfs: pool %s cannot close from state %s", poolID, poolRecord.State)
	}
	binding := closeBindingOf(state)
	encoded, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("bitfs: encode close binding: %w", err)
	}
	if err := t.Store.SaveExact(poolPath(poolID, poolCloseBindingName), append(encoded, '\n')); err != nil {
		return nil, err
	}
	artifact, err := buyer.PrepareCloseArtifact(ctx, t.Facts(), buyer.PrepareCloseInput{
		Pool:                       state.Evidence,
		TargetSellerAmountSatoshis: protocol.Satoshis(state.SellerAmountSatoshis),
	}, t.Signer)
	if err != nil {
		return nil, fmt.Errorf("bitfs: prepare close artifact: %w", err)
	}
	kind12 := artifact.Bytes()
	if err := t.Store.SaveExact(poolPath(poolID, poolKind12Name), kind12); err != nil {
		return nil, err
	}
	return &ClosePreparation{
		PoolID:                    poolID,
		Kind12:                    kind12,
		AuthorizationID:           state.AuthorizationID,
		PaymentSequence:           state.PaymentSequence,
		SellerAmountAfterSatoshis: state.SellerAmountSatoshis,
	}, nil
}

// CompleteClose verifies exact Kind 13 against the bound local state, persists
// the close transaction in the ledger and tries to observe it. Until the
// seller-broadcast transaction is observed the pool stays recovery-pending.
func (t *BuyerTask) CompleteClose(ctx context.Context, poolID string, kind13 []byte) (*CloseCompletion, error) {
	if t.Signer == nil || t.Facts == nil || t.Store == nil || t.Ledger == nil {
		return nil, errors.New("bitfs: store, ledger, signer and facts are required")
	}
	if len(kind13) == 0 {
		return nil, errors.New("bitfs: kind13 is required")
	}
	state, err := LoadPoolState(t.Store, poolID)
	if err != nil {
		return nil, err
	}
	binding, err := loadCloseBinding(t.Store, poolID)
	if err != nil {
		return nil, fmt.Errorf("bitfs: close binding is required: %w", err)
	}
	current := closeBindingOf(state)
	if *binding != current {
		return nil, errors.New("bitfs: close binding does not match the current local payment state")
	}
	if _, err := t.Store.Load(poolPath(poolID, poolKind12Name)); err != nil {
		return nil, fmt.Errorf("bitfs: close request is required: %w", err)
	}
	verified, err := buyer.VerifyCompletedCloseArtifact(buyer.VerifyCompletedCloseArtifactInput{
		Pool:        state.Evidence,
		ResponseRaw: kind13,
	})
	if err != nil {
		return nil, fmt.Errorf("bitfs: verify completed close: %w", err)
	}
	closeRaw := append([]byte(nil), verified.RawTx()...)
	txid, err := parseTxID(closeRaw)
	if err != nil {
		return nil, err
	}
	if err := t.Store.SaveExact(poolPath(poolID, poolKind13Name), kind13); err != nil {
		return nil, err
	}
	if err := t.Store.SaveExact(poolPath(poolID, poolCloseTxName), closeRaw); err != nil {
		return nil, err
	}
	if _, err := t.Ledger.Prepare(ledger.KindClose, txid, hex.EncodeToString(closeRaw)); err != nil {
		return nil, err
	}
	if err := t.Ledger.SetPoolTransaction(poolID, ledger.KindClose, txid); err != nil {
		return nil, err
	}
	poolRecord := t.Ledger.Pools()[poolID]
	if poolRecord.State == ledger.PoolClosed {
		if poolRecord.RecoveryTxID != txid {
			return nil, errors.New("bitfs: pool was already closed by another transaction")
		}
		return &CloseCompletion{TxID: txid, Kind13: kind13, Transaction: closeRaw, Closed: true}, nil
	}
	if err := t.Ledger.SetPoolState(poolID, ledger.PoolRecoveryPending); err != nil {
		return nil, err
	}
	closed, err := t.reconcileClose(ctx, poolID, txid, closeRaw)
	if err != nil {
		return nil, err
	}
	return &CloseCompletion{TxID: txid, Kind13: kind13, Transaction: closeRaw, Closed: closed}, nil
}

// ReconcileClose observes an already verified close transaction by txid. It is
// safe to call repeatedly; a missing transaction simply reports false.
func (t *BuyerTask) ReconcileClose(ctx context.Context, poolID string) (bool, error) {
	if t.Ledger == nil {
		return false, errors.New("bitfs: ledger is required")
	}
	poolRecord, ok := t.Ledger.Pools()[poolID]
	if !ok {
		return false, fmt.Errorf("bitfs: unknown pool %s", poolID)
	}
	if poolRecord.State == ledger.PoolClosed {
		return true, nil
	}
	if poolRecord.State != ledger.PoolRecoveryPending || poolRecord.RecoveryTxID == "" {
		return false, fmt.Errorf("bitfs: pool %s has no close transaction to reconcile", poolID)
	}
	record, ok := t.Ledger.Transaction(poolRecord.RecoveryTxID)
	if !ok {
		return false, fmt.Errorf("bitfs: close transaction %s is not in the ledger", poolRecord.RecoveryTxID)
	}
	raw, err := hex.DecodeString(record.RawHex)
	if err != nil {
		return false, fmt.Errorf("bitfs: close transaction bytes are corrupt: %w", err)
	}
	return t.reconcileClose(ctx, poolID, record.TxID, raw)
}

func (t *BuyerTask) reconcileClose(ctx context.Context, poolID, txid string, raw []byte) (bool, error) {
	if t.Chain == nil {
		return false, nil
	}
	observed, err := t.Chain.TransactionRaw(ctx, txid)
	if err != nil {
		if errors.Is(err, woc.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("bitfs: observe close transaction: %w", err)
	}
	if !bytes.Equal(observed, raw) {
		return false, errors.New("bitfs: observed close transaction differs from the saved exact bytes")
	}
	if err := t.Ledger.MarkTransition(txid, ledger.TxObserved, ""); err != nil {
		return false, err
	}
	if err := t.Ledger.SetPoolState(poolID, ledger.PoolClosed); err != nil {
		return false, err
	}
	return true, nil
}
