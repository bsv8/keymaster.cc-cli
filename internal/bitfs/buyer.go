// Package bitfs wires the go-bitfs protocol steps into the km client store,
// fund ledger and wallet.
//
// The buyer flow is persist-before-send: funding bytes, exact Kind 2/3/4
// artifacts and ledger entries are written before anything is sent or
// broadcast. The funding transaction is broadcast only after the seller's
// Kind 3 presign has been verified.
package bitfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/pool"
	"github.com/bsv8/go-bitfs/protocol"

	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
)

// BuyerTask executes the buyer side of one download task.
type BuyerTask struct {
	Wallet       *wallet.Wallet
	Ledger       *ledger.Ledger
	Store        *store.Store
	Signer       protocol.Signer
	Facts        func() protocol.Facts
	MinerFeeRate protocol.SatoshisPerKilobyte
	// Chain is the optional read-only chain port used to observe a
	// seller-broadcast close transaction. Nil keeps pools recovery-pending.
	Chain CloseChain
	// UnresolvedDir is the client-level unresolved history directory; a
	// delivered round whose payment can no longer be authorized is recorded
	// here instead of being silently abandoned.
	UnresolvedDir string
}

// PreparePoolInput carries everything needed to prepare one pool opening.
type PreparePoolInput struct {
	PoolID             string
	SellerID           string
	SessionID          string
	QuoteRaw           []byte
	SellerPublicKey    protocol.PublicKey
	ArbiterPublicKey   protocol.PublicKey
	PoolAmountSatoshis uint64
	ExpiryLockTime     protocol.RefundLockTime
}

// PoolPreparation is the persisted pre-opening state.
type PoolPreparation struct {
	PoolID           string
	SellerID         string
	SessionID        string
	QuoteRaw         []byte
	AmountSatoshis   uint64
	Funding          *wallet.SpendResult
	Opening          buyer.BuyerOpeningEvidence
	Kind2            []byte
	ExpiryLockTime   protocol.RefundLockTime
	ArbiterPublicKey protocol.PublicKey
}

// PoolCompletion is the result of a verified opening.
type PoolCompletion struct {
	Kind4   []byte
	Pool    buyer.BuyerPoolEvidence
	Opening buyer.BuyerOpeningEvidence
}

// PreparePool builds and signs the funding transaction, persists its exact
// bytes, prepares exact Kind 2 and records the pending pool. Nothing is
// broadcast yet.
func (t *BuyerTask) PreparePool(ctx context.Context, input PreparePoolInput) (*PoolPreparation, error) {
	if t.Wallet == nil || t.Ledger == nil || t.Store == nil || t.Signer == nil || t.Facts == nil {
		return nil, errors.New("bitfs: wallet, ledger, store, signer and facts are required")
	}
	if input.PoolID == "" || input.SellerID == "" || input.SessionID == "" {
		return nil, errors.New("bitfs: poolId, sellerId and sessionId are required")
	}
	if input.PoolAmountSatoshis == 0 {
		return nil, errors.New("bitfs: pool amount must be positive")
	}
	// The quote must verify before any funds are committed.
	if _, err := buyer.AcceptQuote(t.Facts(), input.QuoteRaw); err != nil {
		return nil, fmt.Errorf("bitfs: accept quote: %w", err)
	}
	buyerPublicKey := t.Signer.PublicKey()
	keys := pool.MultisigPoolPublicKeys{
		BuyerPublicKey:   buyerPublicKey[:],
		SellerPublicKey:  input.SellerPublicKey[:],
		ArbiterPublicKey: input.ArbiterPublicKey[:],
	}
	lockingScript, err := pool.Build2of3LockingScript(keys)
	if err != nil {
		return nil, fmt.Errorf("bitfs: build pool script: %w", err)
	}
	funding, err := t.Wallet.Build(ctx, []wallet.Output{{
		Satoshis:      input.PoolAmountSatoshis,
		LockingScript: script.NewFromBytes(lockingScript),
	}})
	if err != nil {
		return nil, fmt.Errorf("bitfs: build funding transaction: %w", err)
	}
	// Until the funding bytes are broadcast, the prepared inputs must be
	// released on every failure so a retry can select them again.
	committed := false
	defer func() {
		if !committed {
			t.Wallet.Release(funding)
		}
	}()
	fundingRaw := funding.Transaction.Bytes()
	if err := t.Store.SaveExact("transactions/"+funding.TxID+".bin", fundingRaw); err != nil {
		return nil, err
	}
	if _, err := t.Ledger.Prepare(ledger.KindFunding, funding.TxID, funding.RawHex); err != nil {
		return nil, err
	}
	if err := t.Ledger.EnsurePool(input.PoolID, input.SellerID, input.SessionID, input.PoolAmountSatoshis); err != nil {
		return nil, err
	}
	if err := t.Ledger.SetPoolTransaction(input.PoolID, ledger.KindFunding, funding.TxID); err != nil {
		return nil, err
	}
	artifact, opening, err := buyer.PrepareOpening(ctx, buyer.PrepareOpeningInput{
		QuoteRaw:                        input.QuoteRaw,
		FundingTransactionRaw:           fundingRaw,
		ExpiryLockTime:                  input.ExpiryLockTime,
		MinerFeeRateSatoshisPerKilobyte: t.MinerFeeRate,
		SellerPublicKey:                 input.SellerPublicKey,
		ArbiterPublicKey:                input.ArbiterPublicKey,
	}, t.Signer)
	if err != nil {
		return nil, fmt.Errorf("bitfs: prepare opening: %w", err)
	}
	kind2 := artifact.Bytes()
	if err := t.Store.SaveExact("exact/kind2-"+digestName(kind2), kind2); err != nil {
		return nil, err
	}
	return &PoolPreparation{
		PoolID:           input.PoolID,
		SellerID:         input.SellerID,
		SessionID:        input.SessionID,
		QuoteRaw:         append([]byte(nil), input.QuoteRaw...),
		AmountSatoshis:   input.PoolAmountSatoshis,
		Funding:          funding,
		Opening:          opening,
		Kind2:            kind2,
		ExpiryLockTime:   input.ExpiryLockTime,
		ArbiterPublicKey: input.ArbiterPublicKey,
	}, nil
}

// CompletePool applies the seller's Kind 3, persists the exact artifacts,
// broadcasts the already-persisted funding transaction and opens the pool in
// the ledger.
func (t *BuyerTask) CompletePool(ctx context.Context, preparation *PoolPreparation, kind3 []byte) (*PoolCompletion, error) {
	if preparation == nil {
		return nil, errors.New("bitfs: preparation is required")
	}
	if len(kind3) == 0 {
		return nil, errors.New("bitfs: kind3 is required")
	}
	// A complete opening that fails before the funding broadcast must release
	// the prepared inputs; after BroadcastFunding the wallet already did.
	broadcastAttempted := false
	defer func() {
		if !broadcastAttempted {
			t.Wallet.Release(preparation.Funding)
		}
	}()
	if err := t.Store.SaveExact("exact/kind3-"+digestName(kind3), kind3); err != nil {
		return nil, err
	}
	opening, poolEvidence, err := buyer.CompleteOpening(preparation.Opening, kind3)
	if err != nil {
		return nil, fmt.Errorf("bitfs: complete opening: %w", err)
	}
	artifact, err := buyer.PrepareFundingDelivery(poolEvidence)
	if err != nil {
		return nil, fmt.Errorf("bitfs: prepare funding delivery: %w", err)
	}
	kind4 := artifact.Bytes()
	if err := t.Store.SaveExact("exact/kind4-"+digestName(kind4), kind4); err != nil {
		return nil, err
	}
	// The completed opening proof and the exact quote are persisted before the
	// funding transaction is broadcast, so a crash can always rebuild the
	// buyer state and never loses the pool evidence.
	verifiedQuote, err := buyer.AcceptQuote(t.Facts(), preparation.QuoteRaw)
	if err != nil {
		return nil, fmt.Errorf("bitfs: re-verify quote for pool metadata: %w", err)
	}
	openingCBOR, err := pool.EncodeOpeningProof(poolEvidence.Opening)
	if err != nil {
		return nil, fmt.Errorf("bitfs: encode opening proof: %w", err)
	}
	if err := savePoolMeta(t.Store, PoolMeta{
		PoolID:         preparation.PoolID,
		SellerID:       preparation.SellerID,
		SessionID:      preparation.SessionID,
		AmountSatoshis: preparation.AmountSatoshis,
		SeedHashHex:    hex.EncodeToString(verifiedQuote.SeedHash()),
		QuoteRawHex:    hex.EncodeToString(preparation.QuoteRaw),
		OpeningCBORHex: hex.EncodeToString(openingCBOR),
		ExpiryLockTime: uint32(preparation.ExpiryLockTime),
	}); err != nil {
		return nil, err
	}
	broadcastAttempted = true
	if err := t.BroadcastFunding(ctx, preparation); err != nil {
		return nil, err
	}
	if err := t.Ledger.SetPoolState(preparation.PoolID, ledger.PoolOpen); err != nil {
		return nil, err
	}
	return &PoolCompletion{Kind4: kind4, Pool: poolEvidence, Opening: opening}, nil
}

// BroadcastFunding (re)broadcasts the exact persisted funding bytes and
// reconciles the ledger: observed on success, result-unknown otherwise.
func (t *BuyerTask) BroadcastFunding(ctx context.Context, preparation *PoolPreparation) error {
	_, err := t.Wallet.BroadcastPrepared(ctx, "pool-funding", preparation.Funding)
	if err != nil {
		if ledgerErr := t.Ledger.MarkTransition(preparation.Funding.TxID, ledger.TxResultUnknown, err.Error()); ledgerErr != nil {
			return fmt.Errorf("bitfs: record unknown funding: %w", ledgerErr)
		}
		return fmt.Errorf("bitfs: broadcast funding: %w", err)
	}
	if err := t.Ledger.MarkTransition(preparation.Funding.TxID, ledger.TxObserved, ""); err != nil {
		return err
	}
	return nil
}

// parseTxID computes the canonical txid of exact transaction bytes.
func parseTxID(raw []byte) (string, error) {
	parsed, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return "", fmt.Errorf("bitfs: parse transaction: %w", err)
	}
	return parsed.TxID().String(), nil
}

// Loading helpers for recovery: the caller rebuilds a preparation from the
// store and ledger after a restart.

// StoredFunding returns the persisted raw funding bytes for a txid.
func StoredFunding(s *store.Store, txid string) ([]byte, error) {
	return s.Load("transactions/" + txid + ".bin")
}

// DigestName exposes the artifact file name scheme for callers that verify
// persisted exact bytes.
func DigestName(data []byte) string { return digestName(data) }

func digestName(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
