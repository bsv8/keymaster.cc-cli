// Buyer purchase driver over one exact-artifact session.
//
// It is the transport-level counterpart of the scheduler command ports: given
// a quote from the seller it opens the pool, buys the seed and every block,
// closes the pool and assembles the file. The scheduler host drives the same
// execution layer step by step; this driver exists so a plain `km <seed>` can
// complete a purchase without a running scheduler loop.
package bitfs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	masterseed "github.com/bsv8/MasterSeed"
	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/protocol"
	"github.com/bsv8/go-bitfs/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/output"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

// PurchaseSession is the minimal exact-artifact stream the buyer needs.
type PurchaseSession interface {
	Read() (wire.Artifact, error)
	Write(wire.Artifact) error
	Close() error
}

// PurchaseOptions configures one direct purchase.
type PurchaseOptions struct {
	PoolID             string
	SeedHashHex        string
	PoolAmountSatoshis uint64
	RefundDelayBlocks  uint32
	OutputDir          string
	PreferredName      string
	// Progress observes stages for the download interface; it must not block.
	Progress func(PurchaseProgress)
	// Resume continues an already funded pool after a transport disconnect:
	// the opening is skipped and only missing content is purchased.
	Resume bool
}

// PurchaseProgress is one stage update.
type PurchaseProgress struct {
	Stage       string
	Detail      string
	BlocksDone  int
	BlocksTotal int
}

// PurchaseResult is one completed purchase.
type PurchaseResult struct {
	PoolID           string
	Path             string
	Bytes            int64
	Rounds           int
	Committed        uint64
	RecommendedNames []string
}

const defaultPurchaseFeeReserve = 2000

// RunPurchase drives the buyer side of one session: quote, opening, seed,
// blocks, close and file assembly.
func (t *BuyerTask) RunPurchase(ctx context.Context, session PurchaseSession, options PurchaseOptions) (*PurchaseResult, error) {
	if t.Wallet == nil || t.Ledger == nil || t.Store == nil || t.Signer == nil || t.Facts == nil {
		return nil, errors.New("bitfs: wallet, ledger, store, signer and facts are required")
	}
	if session == nil {
		return nil, errors.New("bitfs: session is required")
	}
	if options.PoolID == "" {
		return nil, errors.New("bitfs: pool id is required")
	}
	quoteArtifact, err := session.Read()
	if err != nil {
		return nil, fmt.Errorf("bitfs: read quote: %w", err)
	}
	if quoteArtifact.Kind() != wire.FileQuote {
		return nil, fmt.Errorf("bitfs: expected a Kind 1 quote, got kind %d", quoteArtifact.Kind())
	}
	facts := t.Facts()
	var verified *content.VerifiedQuote
	if options.Resume {
		// Restoring an existing contract must not be blocked by the quote's
		// wall-clock expiry; expiry only gates new authorizations.
		verified, err = verifiedQuoteForRestore(quoteArtifact.Bytes())
	} else {
		verified, err = buyer.AcceptQuote(facts, quoteArtifact.Bytes())
	}
	if err != nil {
		return nil, fmt.Errorf("bitfs: accept quote: %w", err)
	}
	quoteExpired := verified.Terms().QuoteExpiresAtUnixSeconds <= facts.Now.Unix()
	if hex.EncodeToString(verified.SeedHash()) != options.SeedHashHex {
		return nil, errors.New("bitfs: quote is for another seed")
	}
	signerPublicKey := t.Signer.PublicKey()
	if string(verified.BuyerPublicKey()) != string(signerPublicKey[:]) {
		return nil, errors.New("bitfs: quote is bound to another buyer key")
	}
	if options.Progress != nil {
		options.Progress(PurchaseProgress{
			Stage:  "quote",
			Detail: fmt.Sprintf("%d 字节 / %d 块", verified.Terms().FileSizeBytes, masterseed.BlockCountForSourceSize(verified.Terms().FileSizeBytes)),
		})
	}
	arbiters := verified.SupportedArbiterPublicKeys()
	if len(arbiters) == 0 {
		return nil, errors.New("bitfs: quote offers no arbiter")
	}
	arbiter, err := protocol.PublicKeyFromBytes(arbiters[0])
	if err != nil {
		return nil, fmt.Errorf("bitfs: invalid arbiter key: %w", err)
	}
	sellerPublicKey, err := protocol.PublicKeyFromBytes(verified.SellerPublicKey())
	if err != nil {
		return nil, fmt.Errorf("bitfs: invalid seller key: %w", err)
	}
	amount := options.PoolAmountSatoshis
	if amount == 0 {
		amount, err = purchasePoolAmount(verified.Terms())
		if err != nil {
			return nil, err
		}
	}
	delay := options.RefundDelayBlocks
	if delay == 0 {
		delay = 5
	}
	lockHeight := int64(facts.BlockHeight) + int64(delay)
	if lockHeight <= 0 || lockHeight >= 500_000_000 {
		return nil, errors.New("bitfs: refund lock height is out of range")
	}
	var resumeState *PoolState
	if options.Resume {
		resumeState, err = LoadPoolState(t.Store, options.PoolID)
		if err != nil {
			return nil, fmt.Errorf("bitfs: resume requires a saved pool: %w", err)
		}
		savedQuote, err := resumeState.Meta.QuoteRaw()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(savedQuote, quoteArtifact.Bytes()) {
			return nil, errors.New("bitfs: resume quote differs from the saved pool quote")
		}
		if _, ok := t.Ledger.Pools()[options.PoolID]; !ok {
			return nil, errors.New("bitfs: resume requires the pool in the ledger")
		}
		if err := t.reconcileLedgerFromJournal(options.PoolID, resumeState); err != nil {
			return nil, err
		}
	} else {
		preparation, err := t.PreparePool(ctx, PreparePoolInput{
			PoolID:             options.PoolID,
			SellerID:           hex.EncodeToString(verified.SellerPublicKey()),
			SessionID:          options.PoolID,
			QuoteRaw:           quoteArtifact.Bytes(),
			SellerPublicKey:    sellerPublicKey,
			ArbiterPublicKey:   arbiter,
			PoolAmountSatoshis: amount,
			ExpiryLockTime:     protocol.RefundLockTime(uint32(lockHeight)),
		})
		if err != nil {
			return nil, err
		}
		kind2, err := wire.ParseAs(wire.RefundPresignRequest, preparation.Kind2)
		if err != nil {
			return nil, err
		}
		if err := session.Write(kind2); err != nil {
			return nil, fmt.Errorf("bitfs: send kind2: %w", err)
		}
		kind3, err := session.Read()
		if err != nil {
			return nil, fmt.Errorf("bitfs: read kind3: %w", err)
		}
		if kind3.Kind() != wire.RefundPresignResponse {
			return nil, fmt.Errorf("bitfs: expected Kind 3, got kind %d", kind3.Kind())
		}
		completion, err := t.CompletePool(ctx, preparation, kind3.Bytes())
		if err != nil {
			return nil, err
		}
		if options.Progress != nil {
			options.Progress(PurchaseProgress{Stage: "opening"})
		}
		kind4, err := wire.ParseAs(wire.FundingTransactionDelivery, completion.Kind4)
		if err != nil {
			return nil, err
		}
		if err := session.Write(kind4); err != nil {
			return nil, fmt.Errorf("bitfs: send kind4: %w", err)
		}
	} // end of non-resume opening

	rounds := 0
	// On resume the seller may have missed the last Kind 7 (the disconnect can
	// happen right after the delivery frame). Replaying the latest saved
	// payment update is idempotent on the seller and lets it advance before
	// the next authorization.
	if options.Resume && resumeState != nil && len(resumeState.Journal) > 0 {
		last := resumeState.Journal[len(resumeState.Journal)-1]
		kind7Raw, err := t.Store.Load(last.Kind7Path)
		if err != nil {
			return nil, fmt.Errorf("bitfs: resume payment evidence is missing: %w", err)
		}
		kind7Artifact, err := wire.ParseAs(wire.PaymentUpdate, kind7Raw)
		if err != nil {
			return nil, err
		}
		if err := session.Write(kind7Artifact); err != nil {
			return nil, fmt.Errorf("bitfs: replay payment update: %w", err)
		}
	}

	// A crash between staging the verified bytes and appending the payment
	// journal leaves a saved Kind 5. Complete it while still deliverable; a
	// delivered round whose authorization expired is an unresolved payment
	// responsibility, never a silent abandonment.
	if pending, err := PendingContentRequest(t.Store, options.PoolID); err != nil {
		return nil, err
	} else if pending != nil {
		authorizationID, err := AuthorizationIDOfKind5(pending)
		if err != nil {
			return nil, err
		}
		_, authorization, err := decodeKind5(pending)
		if err != nil {
			return nil, fmt.Errorf("bitfs: pending kind5 is invalid: %w", err)
		}
		hashes, err := content.DecodeContentHashes(authorization.ContentHashesCBOR)
		if err != nil {
			return nil, err
		}
		if authorization.DeliveryDeadlineUnixSeconds <= facts.Now.Unix() {
			// A verified delivery bundle lets the round be completed (staged,
			// journaled and paid) even after the deadline, including a crash
			// with only part of the batch staged.
			completed, err := t.completePendingFromBundle(ctx, session, options.PoolID, authorizationID, pending)
			if err != nil {
				return nil, err
			}
			if !completed {
				if anyStaged(t.Store, hashes) {
					if quoteExpired {
						if err := t.recordRoundResponsibility(options.PoolID, authorizationID, hashes, "delivered round has no recoverable bundle and its payment authorization expired"); err != nil {
							return nil, err
						}
						return nil, fmt.Errorf("bitfs: delivered round %s cannot be completed after the quote expired; pool left open and responsibility recorded", authorizationID)
					}
					// Delivered while a fresh authorization is still possible:
					// abandon the expired request and buy the batch again.
					if err := abandonPending(t.Store, options.PoolID, authorizationID); err != nil {
						return nil, err
					}
				} else if err := abandonPending(t.Store, options.PoolID, authorizationID); err != nil {
					return nil, err
				}
			}
		} else {
			var replaySeed []byte
			if staged, err := t.Store.Load("staging/seed"); err == nil {
				replaySeed = staged
			}
			if err := t.replayPendingRound(ctx, session, options.PoolID, pending, replaySeed); err != nil {
				return nil, fmt.Errorf("bitfs: complete the pending round: %w", err)
			}
		}
	}

	currentState, err := LoadPoolState(t.Store, options.PoolID)
	if err != nil {
		return nil, err
	}
	if err := t.reconcileLedgerFromJournal(options.PoolID, currentState); err != nil {
		return nil, err
	}
	purchased, err := purchasedHashes(t.Store, currentState.Journal)
	if err != nil {
		return nil, err
	}
	seedHash := verified.SeedHash()
	seedKey := hex.EncodeToString(seedHash)

	// An expired quote cannot authorize new content; the pool still closes and
	// already-paid content is finalized (checked below).
	if !quoteExpired {
		if !purchased[seedKey] {
			if options.Progress != nil {
				options.Progress(PurchaseProgress{Stage: "seed"})
			}
			if err := t.exchangeContent(ctx, session, options.PoolID, [][]byte{seedHash}, nil, verified, &rounds); err != nil {
				return nil, err
			}
		}
		seed, err := t.Store.Load("staging/seed")
		if err != nil {
			return nil, fmt.Errorf("bitfs: seed was not staged: %w", err)
		}
		blockCount := masterseed.BlockCountForSourceSize(verified.Terms().FileSizeBytes)
		blockHashes := make([][]byte, 0, blockCount)
		for index := uint64(0); index < blockCount; index++ {
			hash, err := masterseed.ReadBlockHash(ctx, bytes.NewReader(seed), uint64(len(seed)), index)
			if err != nil {
				return nil, fmt.Errorf("bitfs: read seed block %d: %w", index, err)
			}
			blockHashes = append(blockHashes, hash.Bytes())
		}
		maxBatch := verified.Terms().MaxBlocksPerBatch
		if maxBatch == 0 {
			maxBatch = 64
		}
		missing := make([][]byte, 0, len(blockHashes))
		done := 0
		for _, hash := range blockHashes {
			if purchased[hex.EncodeToString(hash)] {
				done++
				continue
			}
			missing = append(missing, hash)
		}
		for start := 0; start < len(missing); start += int(maxBatch) {
			end := start + int(maxBatch)
			if end > len(missing) {
				end = len(missing)
			}
			if err := t.exchangeContent(ctx, session, options.PoolID, missing[start:end], seed, verified, &rounds); err != nil {
				return nil, fmt.Errorf("bitfs: block batch %d: %w", start, err)
			}
			done += end - start
			if options.Progress != nil {
				options.Progress(PurchaseProgress{Stage: "blocks", BlocksDone: done, BlocksTotal: len(blockHashes)})
			}
		}
	}

	// Success requires every content hash to be covered by a journaled payment
	// round, not merely present in staging.
	currentState, err = LoadPoolState(t.Store, options.PoolID)
	if err != nil {
		return nil, err
	}
	if err := t.reconcileLedgerFromJournal(options.PoolID, currentState); err != nil {
		return nil, err
	}
	purchased, err = purchasedHashes(t.Store, currentState.Journal)
	if err != nil {
		return nil, err
	}
	allPurchased := purchased[seedKey]
	var stagedUnpaid [][]byte
	if !allPurchased {
		if exists, _ := t.Store.Exists("staging/" + seedKey); exists {
			stagedUnpaid = append(stagedUnpaid, seedHash)
		}
	}
	seed, err := t.Store.Load("staging/seed")
	if err == nil {
		blockCount := masterseed.BlockCountForSourceSize(verified.Terms().FileSizeBytes)
		for index := uint64(0); index < blockCount; index++ {
			hash, err := masterseed.ReadBlockHash(ctx, bytes.NewReader(seed), uint64(len(seed)), index)
			if err != nil {
				break
			}
			key := hex.EncodeToString(hash.Bytes())
			if !purchased[key] {
				allPurchased = false
				if exists, _ := t.Store.Exists("staging/" + key); exists {
					stagedUnpaid = append(stagedUnpaid, hash.Bytes())
				}
			}
		}
	} else if !allPurchased {
		allPurchased = false
	}
	if len(stagedUnpaid) > 0 {
		// Delivered but never journaled content means an unresolved payment
		// responsibility: never run the normal close path in that state.
		if err := t.recordRoundResponsibility(options.PoolID, "", stagedUnpaid, "staged content was delivered but never journaled"); err != nil {
			return nil, err
		}
		return nil, errors.New("bitfs: delivered content has no committed payment; pool left open and responsibility recorded")
	}

	if options.Progress != nil {
		options.Progress(PurchaseProgress{Stage: "closing"})
	}
	closePreparation, err := t.PrepareClose(ctx, options.PoolID)
	if err != nil {
		return nil, err
	}
	kind12, err := wire.ParseAs(wire.PoolCloseRequest, closePreparation.Kind12)
	if err != nil {
		return nil, err
	}
	if err := session.Write(kind12); err != nil {
		return nil, fmt.Errorf("bitfs: send kind12: %w", err)
	}
	kind13, err := session.Read()
	if err != nil {
		return nil, fmt.Errorf("bitfs: read kind13: %w", err)
	}
	if kind13.Kind() != wire.PoolCloseResponse {
		return nil, fmt.Errorf("bitfs: expected Kind 13, got kind %d", kind13.Kind())
	}
	closed, err := t.CompleteClose(ctx, options.PoolID, kind13.Bytes())
	if err != nil {
		return nil, err
	}
	poolRecord := t.Ledger.Pools()[options.PoolID]
	result := &PurchaseResult{PoolID: options.PoolID, Committed: poolRecord.Committed}
	if !closed.Closed {
		// The seller broadcasts before answering, but mempool propagation to
		// the chain API is not instant: poll for a bounded window.
		observed := false
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			if confirmed, err := t.ReconcileClose(ctx, options.PoolID); err == nil && confirmed {
				observed = true
				break
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
		if !observed {
			return result, errors.New("bitfs: close transaction was not observed")
		}
	}
	if !allPurchased {
		return result, errors.New("bitfs: content is incomplete; pool closed but the file was not finalized")
	}
	if options.Progress != nil {
		options.Progress(PurchaseProgress{Stage: "assembled"})
	}
	outputDir := options.OutputDir
	if outputDir == "" {
		outputDir = "."
	}
	assembler, err := output.New(t.Store, outputDir)
	if err != nil {
		return result, err
	}
	preferred := options.PreferredName
	if preferred == "" {
		preferred = verified.Terms().RecommendedFilename
	}
	assembled, err := assembler.Complete(ctx, options.SeedHashHex, preferred)
	if err != nil {
		return result, err
	}
	result.Path = assembled.Path
	result.Bytes = assembled.Bytes
	return result, nil
}

// verifiedQuoteForRestore verifies a saved quote as existing contract evidence
// without applying the current-time expiry rule (expiry only gates new
// authorizations).
func verifiedQuoteForRestore(raw []byte) (*content.VerifiedQuote, error) {
	artifact, err := wire.ParseAs(wire.FileQuote, raw)
	if err != nil {
		return nil, err
	}
	quote, err := wire.DecodeFileQuote(artifact)
	if err != nil {
		return nil, err
	}
	return content.VerifyQuoteEvidence(quote)
}

// replayPendingRound completes an authorization that was saved before a crash
// but never journaled. The seller persists its exact Kind 6, so replaying the
// same Kind 5 yields the same verified delivery.
func (t *BuyerTask) replayPendingRound(ctx context.Context, session PurchaseSession, poolID string, kind5, seed []byte) error {
	artifact, err := wire.ParseAs(wire.ContentRequest, kind5)
	if err != nil {
		return err
	}
	if err := session.Write(artifact); err != nil {
		return fmt.Errorf("bitfs: replay pending kind5: %w", err)
	}
	kind6, err := session.Read()
	if err != nil {
		return fmt.Errorf("bitfs: read replayed kind6: %w", err)
	}
	if kind6.Kind() != wire.ContentDelivery {
		return fmt.Errorf("bitfs: expected Kind 6, got kind %d", kind6.Kind())
	}
	result, err := t.CompleteDelivery(ctx, DeliveryInput{PoolID: poolID, Kind5: kind5, Kind6: kind6.Bytes(), Seed: seed})
	if err != nil {
		return err
	}
	kind7, err := wire.ParseAs(wire.PaymentUpdate, result.Kind7)
	if err != nil {
		return err
	}
	if err := session.Write(kind7); err != nil {
		return fmt.Errorf("bitfs: send replayed kind7: %w", err)
	}
	return nil
}

// exchangeContent runs one Kind 5/6/7 round and stages the payloads.
func (t *BuyerTask) exchangeContent(ctx context.Context, session PurchaseSession, poolID string, hashes [][]byte, seed []byte, quote *content.VerifiedQuote, rounds *int) error {
	now := t.Facts().Now.Unix()
	deadline := now + 60
	if expires := quote.Terms().QuoteExpiresAtUnixSeconds; deadline > expires {
		deadline = expires
	}
	if deadline <= now {
		return errors.New("bitfs: quote expired before the next authorization")
	}
	request, err := t.PrepareContentRequest(ctx, ContentRequestInput{
		PoolID:           poolID,
		ContentHashes:    hashes,
		DeliveryDeadline: content.UnixSeconds(deadline),
		Seed:             seed,
	})
	if err != nil {
		return err
	}
	kind5, err := wire.ParseAs(wire.ContentRequest, request.Kind5)
	if err != nil {
		return err
	}
	if err := session.Write(kind5); err != nil {
		return fmt.Errorf("bitfs: send kind5: %w", err)
	}
	kind6, err := session.Read()
	if err != nil {
		return fmt.Errorf("bitfs: read kind6: %w", err)
	}
	if kind6.Kind() != wire.ContentDelivery {
		return fmt.Errorf("bitfs: expected Kind 6, got kind %d", kind6.Kind())
	}
	result, err := t.CompleteDelivery(ctx, DeliveryInput{
		PoolID: poolID, Kind5: request.Kind5, Kind6: kind6.Bytes(), Seed: seed,
	})
	if err != nil {
		return err
	}
	kind7, err := wire.ParseAs(wire.PaymentUpdate, result.Kind7)
	if err != nil {
		return err
	}
	// CompleteDelivery already staged the verified payloads before advancing
	// the payment journal; only the payment update leaves the process now.
	if err := session.Write(kind7); err != nil {
		return fmt.Errorf("bitfs: send kind7: %w", err)
	}
	*rounds++
	return nil
}

// purchasePoolAmount budgets a pool that covers every content price plus a
// miner-fee reserve for the in-pool payment transactions.
func purchasePoolAmount(terms *content.FileQuoteTerms) (uint64, error) {
	total := terms.SeedPriceSatoshis
	blockCount := masterseed.BlockCountForSourceSize(terms.FileSizeBytes)
	for index := uint64(0); index < blockCount; index++ {
		size, err := masterseed.ExpectedBlockSize(terms.FileSizeBytes, index)
		if err != nil {
			return 0, err
		}
		price, err := content.BlockPriceSatoshis(terms.FullBlockPriceSatoshis, size)
		if err != nil {
			return 0, err
		}
		total += price
	}
	return total + defaultPurchaseFeeReserve, nil
}

// anyStaged reports whether at least one of the hashes has verified bytes.
func anyStaged(s *store.Store, hashes [][]byte) bool {
	for _, hash := range hashes {
		if exists, err := s.Exists("staging/" + hex.EncodeToString(hash)); err == nil && exists {
			return true
		}
	}
	return false
}

// completePendingFromBundle finishes an authorization from its persisted
// verified delivery bundle: stage the payloads, append the journal, reconcile
// the ledger and send the saved Kind 7. This is the recovery path for partial
// staging and expired deadlines; it never re-verifies time-gated evidence.
func (t *BuyerTask) completePendingFromBundle(ctx context.Context, session PurchaseSession, poolID, authorizationID string, pending []byte) (bool, error) {
	bundle, err := LoadDeliveryBundle(t.Store, poolID, authorizationID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	state, err := LoadPoolState(t.Store, poolID)
	if err != nil {
		return false, err
	}
	authorization, payloads, err := t.validateDeliveryBundle(state, authorizationID, pending, bundle)
	if err != nil {
		return false, err
	}
	recorded, _, err := findJournalEntry(state.Journal, authorizationID)
	if err != nil {
		return false, err
	}
	kind5Path := poolPath(poolID, kindName("kind5", authorizationID))
	kind6Path := poolPath(poolID, kindName("kind6", authorizationID))
	kind7Path := poolPath(poolID, kindName("kind7", authorizationID))
	if err := t.Store.SaveExact(kind6Path, bundle.Kind6); err != nil {
		return false, err
	}
	if err := t.Store.SaveExact(kind7Path, bundle.Kind7); err != nil {
		return false, err
	}
	if err := t.stageDelivery(state, authorization, payloads); err != nil {
		return false, err
	}
	if recorded == nil {
		if err := appendJournal(t.Store, poolID, PaymentJournalEntry{
			AuthorizationID:      authorizationID,
			PaymentSequence:      authorization.PaymentSequence,
			SellerAmountSatoshis: authorization.SellerAmountAfterSatoshis,
			Kind5Path:            kind5Path,
			Kind6Path:            kind6Path,
			Kind7Path:            kind7Path,
			RecordedAt:           recordedAt(),
		}); err != nil {
			return false, err
		}
	}
	if err := t.Ledger.ReconcilePoolCommitted(poolID, authorization.SellerAmountAfterSatoshis); err != nil {
		return false, err
	}
	artifact, err := wire.ParseAs(wire.PaymentUpdate, bundle.Kind7)
	if err != nil {
		return false, err
	}
	if err := session.Write(artifact); err != nil {
		return false, fmt.Errorf("bitfs: send recovered kind7: %w", err)
	}
	return true, nil
}
