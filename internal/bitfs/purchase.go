// Buyer content purchase: exact Kind 5 content request, exact Kind 6 delivery
// verification and the exact Kind 7 buyer payment update.
//
// Pool payments are never broadcast: the buyer's local Kind 5/7 journal is the
// progression state for the next round and for the close. Everything that
// leaves the process is persisted first; every replay uses saved bytes.
package bitfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/protocol"
	"github.com/bsv8/go-bitfs/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/state"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

// ContentRequestInput selects one batch to purchase from an open pool.
type ContentRequestInput struct {
	PoolID           string
	ContentHashes    [][]byte
	DeliveryDeadline content.UnixSeconds
	// Seed is the verified seed bytes; required whenever the batch contains a
	// block and omitted for a pure seed batch.
	Seed []byte
}

// ContentRequest is one exact Kind 5 ready to send.
type ContentRequest struct {
	PoolID                    string
	Kind5                     []byte
	AuthorizationID           string
	PaymentSequence           uint32
	SellerAmountAfterSatoshis uint64
	// Replayed reports that an already saved pending request was returned
	// instead of signing a new authorization.
	Replayed bool
}

// PrepareContentRequest builds the next round's exact Kind 5 against the
// newest provable pool state, persists it and returns it. If an undelivered
// request for the same batch exists, the saved bytes are replayed; a pending
// request for a different batch is an error rather than a silent overwrite.
func (t *BuyerTask) PrepareContentRequest(ctx context.Context, input ContentRequestInput) (*ContentRequest, error) {
	if t.Signer == nil || t.Facts == nil || t.Store == nil {
		return nil, errors.New("bitfs: store, signer and facts are required")
	}
	if len(input.ContentHashes) == 0 {
		return nil, errors.New("bitfs: at least one content hash is required")
	}
	state, err := LoadPoolState(t.Store, input.PoolID)
	if err != nil {
		return nil, err
	}
	encodedHashes, err := content.EncodeContentHashes(input.ContentHashes)
	if err != nil {
		return nil, fmt.Errorf("bitfs: encode content hashes: %w", err)
	}
	if pending, err := PendingContentRequest(t.Store, input.PoolID); err != nil {
		return nil, err
	} else if pending != nil {
		_, authorization, err := decodeKind5(pending)
		if err != nil {
			return nil, fmt.Errorf("bitfs: pending request is corrupt: %w", err)
		}
		if !bytes.Equal(authorization.ContentHashesCBOR, encodedHashes) ||
			authorization.DeliveryDeadlineUnixSeconds != int64(input.DeliveryDeadline) {
			return nil, errors.New("bitfs: a pending content request for another batch must be completed first")
		}
		authorizationID, err := AuthorizationIDOfKind5(pending)
		if err != nil {
			return nil, err
		}
		return &ContentRequest{
			PoolID:                    input.PoolID,
			Kind5:                     pending,
			AuthorizationID:           authorizationID,
			PaymentSequence:           authorization.PaymentSequence,
			SellerAmountAfterSatoshis: authorization.SellerAmountAfterSatoshis,
			Replayed:                  true,
		}, nil
	}
	quoteRaw, err := state.Meta.QuoteRaw()
	if err != nil {
		return nil, err
	}
	artifact, _, err := buyer.PrepareContentRequest(ctx, t.Facts(), buyer.RequestContentInput{
		QuoteRaw:         quoteRaw,
		Pool:             state.Evidence,
		ContentHashes:    input.ContentHashes,
		DeliveryDeadline: input.DeliveryDeadline,
		Seed:             input.Seed,
	}, t.Signer)
	if err != nil {
		return nil, fmt.Errorf("bitfs: prepare content request: %w", err)
	}
	kind5 := artifact.Bytes()
	_, authorization, err := decodeKind5(kind5)
	if err != nil {
		return nil, fmt.Errorf("bitfs: decode own content request: %w", err)
	}
	authorizationID, err := AuthorizationIDOfKind5(kind5)
	if err != nil {
		return nil, err
	}
	if authorization.PaymentSequence != state.PaymentSequence+1 {
		return nil, fmt.Errorf("bitfs: content request sequence %d does not extend %d", authorization.PaymentSequence, state.PaymentSequence)
	}
	if err := t.Store.SaveExact(poolPath(input.PoolID, kindName("kind5", authorizationID)), kind5); err != nil {
		return nil, err
	}
	return &ContentRequest{
		PoolID:                    input.PoolID,
		Kind5:                     kind5,
		AuthorizationID:           authorizationID,
		PaymentSequence:           authorization.PaymentSequence,
		SellerAmountAfterSatoshis: authorization.SellerAmountAfterSatoshis,
	}, nil
}

// DeliveryInput carries the exact request and the seller's exact delivery.
type DeliveryInput struct {
	PoolID string
	// Kind5 is the exact request bytes sent to the seller; empty means the
	// saved pending request.
	Kind5 []byte
	Kind6 []byte
	Seed  []byte
}

// DeliveryBundle is the atomically persisted, already-verified delivery before
// per-block staging: recovery can finish the round (stage, journal, pay) even
// after the authorization deadline, without re-verifying time-gated evidence.
type DeliveryBundle struct {
	AuthorizationID           string   `json:"authorizationId"`
	PaymentSequence           uint32   `json:"paymentSequence"`
	SellerAmountAfterSatoshis uint64   `json:"sellerAmountAfterSatoshis"`
	Kind6                     []byte   `json:"kind6"`
	Kind7                     []byte   `json:"kind7"`
	Payloads                  [][]byte `json:"payloads"`
}

// DeliveryResult is one verified and recorded content round.
type DeliveryResult struct {
	Payloads                  [][]byte
	Kind7                     []byte
	AuthorizationID           string
	PaymentSequence           uint32
	SellerAmountAfterSatoshis uint64
	CommittedSatoshis         uint64
}

// CompleteDelivery verifies exact Kind 6 against the current pool state,
// persists the round evidence, appends the payment journal and commits the
// cumulative ledger payment. Re-running for an already journaled
// authorization is idempotent: it re-verifies and returns the saved Kind 7
// without charging the pool twice.
func (t *BuyerTask) CompleteDelivery(ctx context.Context, input DeliveryInput) (*DeliveryResult, error) {
	if t.Signer == nil || t.Facts == nil || t.Store == nil || t.Ledger == nil {
		return nil, errors.New("bitfs: store, ledger, signer and facts are required")
	}
	if len(input.Kind6) == 0 {
		return nil, errors.New("bitfs: kind6 is required")
	}
	state, err := LoadPoolState(t.Store, input.PoolID)
	if err != nil {
		return nil, err
	}
	pending, err := PendingContentRequest(t.Store, input.PoolID)
	if err != nil {
		return nil, err
	}
	kind5 := input.Kind5
	if len(kind5) == 0 {
		if pending == nil {
			return nil, errors.New("bitfs: no pending content request for this pool")
		}
		kind5 = pending
	}
	authorizationID, err := AuthorizationIDOfKind5(kind5)
	if err != nil {
		return nil, fmt.Errorf("bitfs: kind5 is invalid: %w", err)
	}
	_, authorization, err := decodeKind5(kind5)
	if err != nil {
		return nil, fmt.Errorf("bitfs: kind5 is invalid: %w", err)
	}
	recorded, recordedIndex, err := findJournalEntry(state.Journal, authorizationID)
	if err != nil {
		return nil, err
	}
	if recorded == nil && (pending == nil || !bytes.Equal(pending, kind5)) {
		return nil, errors.New("bitfs: kind5 is neither the pending request nor a recorded round")
	}
	// A finished round must be verified against the state it extended, not
	// against the newest state that already contains it.
	verificationPool := state.Evidence
	if recorded != nil {
		if verificationPool, err = evidenceBefore(t.Store, state, recordedIndex); err != nil {
			return nil, err
		}
	}
	quoteRaw, err := state.Meta.QuoteRaw()
	if err != nil {
		return nil, err
	}
	payloads, artifact, err := buyer.VerifyDelivery(ctx, t.Facts(), buyer.VerifyDeliveryInput{
		Authorization: buyer.BuyerAuthorizationEvidence{RawKind1: quoteRaw, RawKind5: kind5},
		Pool:          verificationPool,
		DeliveryRaw:   input.Kind6,
		Seed:          input.Seed,
	}, t.Signer)
	if err != nil {
		return nil, fmt.Errorf("bitfs: verify delivery: %w", err)
	}
	kind7 := artifact.Bytes()
	// Persist the verified delivery bundle and exact Kind 6/7 before staging a
	// single payload: a crash anywhere after this point can complete the round
	// from the bundle, including partial staging and expired deadlines.
	bundle := DeliveryBundle{
		AuthorizationID:           authorizationID,
		PaymentSequence:           authorization.PaymentSequence,
		SellerAmountAfterSatoshis: authorization.SellerAmountAfterSatoshis,
		Kind6:                     append([]byte(nil), input.Kind6...),
		Kind7:                     append([]byte(nil), kind7...),
		Payloads:                  clonePayloads(payloads),
	}
	encodedBundle, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("bitfs: encode delivery bundle: %w", err)
	}
	if err := t.Store.Save(poolPath(input.PoolID, "delivered-"+authorizationID+".json"), encodedBundle); err != nil {
		return nil, err
	}
	if err := t.Store.SaveExact(poolPath(input.PoolID, kindName("kind6", authorizationID)), input.Kind6); err != nil {
		return nil, err
	}
	if err := t.Store.SaveExact(poolPath(input.PoolID, kindName("kind7", authorizationID)), kind7); err != nil {
		return nil, err
	}
	// Persist the delivered bytes before advancing any payment state: a crash
	// after this point must never leave a paid round whose content is missing.
	if err := t.stageDelivery(state, authorization, payloads); err != nil {
		return nil, err
	}
	if recorded != nil {
		savedKind7, err := t.Store.Load(recorded.Kind7Path)
		if err != nil {
			return nil, fmt.Errorf("bitfs: load recorded kind7: %w", err)
		}
		if !bytes.Equal(savedKind7, kind7) {
			return nil, errors.New("bitfs: selected authorization is already recorded with a different kind7")
		}
		// The journal append may have succeeded before a crash; reconcile the
		// ledger to the recorded cumulative amount so no round is double
		// counted or lost.
		if err := t.Ledger.ReconcilePoolCommitted(input.PoolID, recorded.SellerAmountSatoshis); err != nil {
			return nil, err
		}
		return &DeliveryResult{
			Payloads:                  payloads,
			Kind7:                     kind7,
			AuthorizationID:           authorizationID,
			PaymentSequence:           recorded.PaymentSequence,
			SellerAmountAfterSatoshis: recorded.SellerAmountSatoshis,
		}, nil
	}
	if uint32(authorization.PaymentSequence) != state.PaymentSequence+1 {
		return nil, fmt.Errorf("bitfs: delivery sequence %d does not extend %d", authorization.PaymentSequence, state.PaymentSequence)
	}
	if authorization.SellerAmountAfterSatoshis < state.SellerAmountSatoshis {
		return nil, errors.New("bitfs: delivery amount regresses the seller amount")
	}
	kind6Path := poolPath(input.PoolID, kindName("kind6", authorizationID))
	kind5Path := poolPath(input.PoolID, kindName("kind5", authorizationID))
	kind7Path := poolPath(input.PoolID, kindName("kind7", authorizationID))
	// The journal is written first: if the process dies before the ledger is
	// reconciled, the retry path above re-derives the cumulative amount from
	// the recorded evidence instead of charging twice.
	if err := appendJournal(t.Store, input.PoolID, PaymentJournalEntry{
		AuthorizationID:      authorizationID,
		PaymentSequence:      authorization.PaymentSequence,
		SellerAmountSatoshis: authorization.SellerAmountAfterSatoshis,
		Kind5Path:            kind5Path,
		Kind6Path:            kind6Path,
		Kind7Path:            kind7Path,
		RecordedAt:           recordedAt(),
	}); err != nil {
		return nil, err
	}
	if err := t.Ledger.ReconcilePoolCommitted(input.PoolID, authorization.SellerAmountAfterSatoshis); err != nil {
		return nil, err
	}
	return &DeliveryResult{
		Payloads:                  payloads,
		Kind7:                     kind7,
		AuthorizationID:           authorizationID,
		PaymentSequence:           authorization.PaymentSequence,
		SellerAmountAfterSatoshis: authorization.SellerAmountAfterSatoshis,
		CommittedSatoshis:         authorization.SellerAmountAfterSatoshis - state.SellerAmountSatoshis,
	}, nil
}

// stageDelivery writes the verified payloads into the staging area (by content
// hash, plus the seed copy) before the payment journal advances.
func (t *BuyerTask) stageDelivery(state *PoolState, authorization *content.PaymentAuthorization, payloads [][]byte) error {
	hashes, err := content.DecodeContentHashes(authorization.ContentHashesCBOR)
	if err != nil {
		return fmt.Errorf("bitfs: decode authorization hashes: %w", err)
	}
	if len(hashes) != len(payloads) {
		return errors.New("bitfs: delivery payload count does not match the authorization")
	}
	for index, hash := range hashes {
		encoded := hex.EncodeToString(hash)
		if err := t.Store.SaveExact("staging/"+encoded, payloads[index]); err != nil {
			return err
		}
		if state.Meta.SeedHashHex != "" && encoded == state.Meta.SeedHashHex {
			if err := t.Store.SaveExact("staging/seed", payloads[index]); err != nil {
				return err
			}
		}
	}
	return nil
}

// purchasedHashes returns every content hash covered by a journaled payment
// round. Staged bytes alone do not prove payment.
func clonePayloads(payloads [][]byte) [][]byte {
	cloned := make([][]byte, 0, len(payloads))
	for _, payload := range payloads {
		cloned = append(cloned, append([]byte(nil), payload...))
	}
	return cloned
}

// LoadDeliveryBundle reads the persisted verified delivery for an authorization.
func LoadDeliveryBundle(s *store.Store, poolID, authorizationID string) (*DeliveryBundle, error) {
	raw, err := s.Load(poolPath(poolID, "delivered-"+authorizationID+".json"))
	if err != nil {
		return nil, err
	}
	var bundle DeliveryBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return nil, fmt.Errorf("bitfs: decode delivery bundle: %w", err)
	}
	if bundle.AuthorizationID != authorizationID {
		return nil, errors.New("bitfs: delivery bundle belongs to another authorization")
	}
	return &bundle, nil
}

func purchasedHashes(s *store.Store, entries []PaymentJournalEntry) (map[string]bool, error) {
	purchased := make(map[string]bool)
	for _, entry := range entries {
		kind5, err := s.Load(entry.Kind5Path)
		if err != nil {
			return nil, fmt.Errorf("bitfs: load journaled kind5: %w", err)
		}
		_, authorization, err := decodeKind5(kind5)
		if err != nil {
			return nil, err
		}
		hashes, err := content.DecodeContentHashes(authorization.ContentHashesCBOR)
		if err != nil {
			return nil, err
		}
		for _, hash := range hashes {
			purchased[hex.EncodeToString(hash)] = true
		}
	}
	return purchased, nil
}

// allStaged reports whether every hash already has verified bytes on disk.
func allStaged(s *store.Store, hashes [][]byte) bool {
	if len(hashes) == 0 {
		return false
	}
	for _, hash := range hashes {
		if exists, err := s.Exists("staging/" + hex.EncodeToString(hash)); err != nil || !exists {
			return false
		}
	}
	return true
}

// recordRoundResponsibility appends a delivered-but-unpayable round to the
// client-level unresolved history; it must never be silently abandoned.
func (t *BuyerTask) recordRoundResponsibility(poolID, authorizationID string, hashes [][]byte, reason string) error {
	if t.UnresolvedDir == "" {
		return nil
	}
	encoded := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		encoded = append(encoded, hex.EncodeToString(hash))
	}
	return state.Append(t.UnresolvedDir, state.Unresolved{
		RecordedAt: time.Now().UTC(),
		Kind:       "bitfs-round-responsibility",
		Reason:     fmt.Sprintf("pool=%s authorization=%s hashes=%s: %s", poolID, authorizationID, strings.Join(encoded, ","), reason),
	})
}

func findJournalEntry(entries []PaymentJournalEntry, authorizationID string) (*PaymentJournalEntry, int, error) {
	for index := range entries {
		if entries[index].AuthorizationID == authorizationID {
			if index != len(entries)-1 {
				return nil, 0, errors.New("bitfs: journal contains entries after a replayed authorization")
			}
			return &entries[index], index, nil
		}
	}
	return nil, 0, nil
}

// evidenceBefore rebuilds the pool evidence that existed before the journal
// entry at index: the previous round's pair, or the initial refund state.
func evidenceBefore(s *store.Store, state *PoolState, index int) (buyer.BuyerPoolEvidence, error) {
	if index == 0 {
		return buyer.BuyerPoolEvidence{Opening: state.Opening}, nil
	}
	previous := state.Journal[index-1]
	kind5, err := s.Load(previous.Kind5Path)
	if err != nil {
		return buyer.BuyerPoolEvidence{}, fmt.Errorf("bitfs: load prior kind5: %w", err)
	}
	kind7, err := s.Load(previous.Kind7Path)
	if err != nil {
		return buyer.BuyerPoolEvidence{}, fmt.Errorf("bitfs: load prior kind7: %w", err)
	}
	return buyer.BuyerPoolEvidence{
		Opening: state.Opening,
		LatestBuyerPayment: &buyer.BuyerPaymentEvidence{
			RawKind5: kind5,
			RawKind7: kind7,
		},
	}, nil
}

// reconcileLedgerFromJournal rebuilds the cumulative committed amount from the
// verified journal. A crash between the journal append and the ledger update
// must never let close/summary use the stale amount.
func (t *BuyerTask) reconcileLedgerFromJournal(poolID string, state *PoolState) error {
	if len(state.Journal) == 0 {
		return nil
	}
	target := state.Journal[len(state.Journal)-1].SellerAmountSatoshis
	if err := t.Ledger.ReconcilePoolCommitted(poolID, target); err != nil {
		return fmt.Errorf("bitfs: reconcile ledger from journal: %w", err)
	}
	return nil
}

// validateDeliveryBundle cross-checks every derived bundle field against the
// exact, time-independent evidence before any state advances: Kind 5 buyer
// signature and quote binding, Kind 6 seller signature and authorization
// binding, payload hashes, sequence/amount, and Kind 7 authorization binding.
// It deliberately ignores current time but never trusts the JSON fields.
func (t *BuyerTask) validateDeliveryBundle(state *PoolState, authorizationID string, pending []byte, bundle *DeliveryBundle) (*content.PaymentAuthorization, [][]byte, error) {
	artifact, err := wire.ParseAs(wire.ContentRequest, pending)
	if err != nil {
		return nil, nil, err
	}
	request, err := wire.DecodeContentRequest(artifact)
	if err != nil {
		return nil, nil, err
	}
	quoteRaw, err := state.Meta.QuoteRaw()
	if err != nil {
		return nil, nil, err
	}
	quote, err := content.DecodeSignedFileQuote(quoteRaw)
	if err != nil {
		return nil, nil, err
	}
	opening, err := state.Meta.OpeningProof()
	if err != nil {
		return nil, nil, err
	}
	authorization, _, err := content.VerifyContentRequestEvidence(request, quote, opening)
	if err != nil {
		return nil, nil, fmt.Errorf("bitfs: delivery bundle kind5 evidence: %w", err)
	}
	computedID, err := AuthorizationIDOfKind5(pending)
	if err != nil {
		return nil, nil, err
	}
	if computedID != authorizationID || bundle.AuthorizationID != computedID {
		return nil, nil, errors.New("bitfs: delivery bundle authorization id mismatch")
	}
	if bundle.PaymentSequence != authorization.PaymentSequence ||
		bundle.SellerAmountAfterSatoshis != authorization.SellerAmountAfterSatoshis {
		return nil, nil, errors.New("bitfs: delivery bundle sequence or amount disagrees with kind5")
	}
	hashes, err := content.DecodeContentHashes(authorization.ContentHashesCBOR)
	if err != nil {
		return nil, nil, err
	}
	typedID, err := content.PaymentAuthorizationID(request.PaymentAuthorizationCBOR)
	if err != nil {
		return nil, nil, err
	}

	kind6Artifact, err := wire.ParseAs(wire.ContentDelivery, bundle.Kind6)
	if err != nil {
		return nil, nil, err
	}
	delivery, err := wire.DecodeContentDelivery(kind6Artifact)
	if err != nil {
		return nil, nil, err
	}
	deliveryID, err := content.DecodeContentDeliveryDocument(delivery.ContentDeliveryCBOR)
	if err != nil {
		return nil, nil, err
	}
	if deliveryID != typedID {
		return nil, nil, errors.New("bitfs: delivery bundle kind6 references another authorization")
	}
	if err := protocol.VerifyWireDocument(opening.OpeningSellerPublicKey(), protocol.WireVersion, 6, delivery.ContentDeliveryCBOR, delivery.SellerContentDeliverySignature); err != nil {
		return nil, nil, fmt.Errorf("bitfs: delivery bundle seller signature: %w", err)
	}
	payloads, err := content.DecodeContentPayloads(delivery.ContentPayloadsCBOR)
	if err != nil {
		return nil, nil, err
	}
	if len(payloads) != len(hashes) || len(bundle.Payloads) != len(hashes) {
		return nil, nil, errors.New("bitfs: delivery bundle payload count disagrees with kind5")
	}
	for index, payload := range payloads {
		digest := sha256.Sum256(payload)
		if !bytes.Equal(digest[:], hashes[index]) {
			return nil, nil, fmt.Errorf("bitfs: delivery bundle payload %d does not hash to its kind5 entry", index)
		}
		if !bytes.Equal(payload, bundle.Payloads[index]) {
			return nil, nil, fmt.Errorf("bitfs: delivery bundle payload %d differs from kind6", index)
		}
	}

	kind7Artifact, err := wire.ParseAs(wire.PaymentUpdate, bundle.Kind7)
	if err != nil {
		return nil, nil, err
	}
	update, err := wire.DecodePaymentUpdate(kind7Artifact)
	if err != nil {
		return nil, nil, err
	}
	if update.PaymentAuthorizationID != typedID {
		return nil, nil, errors.New("bitfs: delivery bundle kind7 references another authorization")
	}
	// Verify the candidate Kind 5/7 pair itself, time-independently: the SDK
	// reconstructs the target payment state and verifies the buyer transaction
	// signature. A valid encoding with a forged signature or a Kind 7 for a
	// different state must be rejected before any journal/ledger advance.
	derivedSequence, derivedAmount, err := buyer.InspectPool(buyer.BuyerPoolEvidence{
		Opening: opening,
		LatestBuyerPayment: &buyer.BuyerPaymentEvidence{
			RawKind5: pending,
			RawKind7: bundle.Kind7,
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("bitfs: delivery bundle kind7 payment evidence: %w", err)
	}
	if uint32(derivedSequence) != authorization.PaymentSequence ||
		uint64(derivedAmount) != authorization.SellerAmountAfterSatoshis {
		return nil, nil, errors.New("bitfs: delivery bundle kind7 does not reconstruct its declared payment state")
	}
	if state.AuthorizationID != authorizationID {
		if authorization.PaymentSequence != state.PaymentSequence+1 {
			return nil, nil, errors.New("bitfs: delivery bundle authorization is not the next payment sequence")
		}
		if authorization.SellerAmountAfterSatoshis < state.SellerAmountSatoshis {
			return nil, nil, errors.New("bitfs: delivery bundle authorization regresses the seller amount")
		}
	}
	for name, exact := range map[string][]byte{"kind6": bundle.Kind6, "kind7": bundle.Kind7} {
		if saved, err := t.Store.Load(poolPath(state.Meta.PoolID, kindName(name, authorizationID))); err == nil && !bytes.Equal(saved, exact) {
			return nil, nil, fmt.Errorf("bitfs: delivery bundle %s differs from the saved exact evidence", name)
		}
	}
	return authorization, payloads, nil
}

// PurchasedHashes returns the content hashes covered by journaled payment
// rounds for one pool. Staged but unjournaled bytes are deliberately excluded.
func PurchasedHashes(s *store.Store, poolID string) (map[string]bool, error) {
	state, err := LoadPoolState(s, poolID)
	if err != nil {
		return nil, err
	}
	return purchasedHashes(s, state.Journal)
}

// JournaledRound returns the journal entry for an authorization, if present.
func JournaledRound(s *store.Store, poolID, authorizationID string) (*PaymentJournalEntry, bool, error) {
	state, err := LoadPoolState(s, poolID)
	if err != nil {
		return nil, false, err
	}
	entry, _, err := findJournalEntry(state.Journal, authorizationID)
	if err != nil {
		return nil, false, err
	}
	return entry, entry != nil, nil
}

// ReconcilePoolLedger rebuilds the ledger's committed amount from the verified
// journal. It is idempotent and safe to call before close or new payments.
func ReconcilePoolLedger(task *BuyerTask, poolID string) error {
	state, err := LoadPoolState(task.Store, poolID)
	if err != nil {
		return err
	}
	return task.reconcileLedgerFromJournal(poolID, state)
}

// kind7Sender adapts a Kind 7 callback to the PurchaseSession interface used by
// the recovery path.
type kind7Sender struct {
	send func([]byte) error
}

func (k kind7Sender) Read() (wire.Artifact, error) {
	return wire.Artifact{}, errors.New("bitfs: recovery session is write-only")
}

func (k kind7Sender) Write(artifact wire.Artifact) error { return k.send(artifact.Bytes()) }

func (k kind7Sender) Close() error { return nil }

// CompletePendingRound finishes a pending round from its verified delivery
// bundle, if one exists. It is the recovery entry for a host that restarted
// between a delivery and its journal: staging, journal, ledger and the saved
// Kind 7 are applied, never re-verified against the current time.
func CompletePendingRound(ctx context.Context, task *BuyerTask, poolID string, send func([]byte) error) (bool, error) {
	pending, err := PendingContentRequest(task.Store, poolID)
	if err != nil || pending == nil {
		return false, err
	}
	authorizationID, err := AuthorizationIDOfKind5(pending)
	if err != nil {
		return false, err
	}
	if _, err := LoadDeliveryBundle(task.Store, poolID, authorizationID); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return task.completePendingFromBundle(ctx, kind7Sender{send: send}, poolID, authorizationID, pending)
}

// ResolveExpiredPending classifies an expired pending authorization. Delivered
// bytes (any staged prefix) are an unresolved responsibility and the pending is
// kept; a pending with no staged bytes is abandoned so the pool can close.
func ResolveExpiredPending(task *BuyerTask, poolID, reason string) (bool, error) {
	pending, err := PendingContentRequest(task.Store, poolID)
	if err != nil || pending == nil {
		return false, err
	}
	authorizationID, err := AuthorizationIDOfKind5(pending)
	if err != nil {
		return false, err
	}
	_, authorization, err := decodeKind5(pending)
	if err != nil {
		return false, err
	}
	hashes, err := content.DecodeContentHashes(authorization.ContentHashesCBOR)
	if err != nil {
		return false, err
	}
	if anyStaged(task.Store, hashes) {
		if err := task.recordRoundResponsibility(poolID, authorizationID, hashes, reason); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, abandonPending(task.Store, poolID, authorizationID)
}
