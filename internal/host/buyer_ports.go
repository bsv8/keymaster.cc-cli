// BuyerPorts implements the kernel command ports with the real buyer
// execution layer. Protocol transport is injected through Peer, so the same
// ports work with a fake peer in tests and with a real libp2p session later.
package host

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	tx "github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/protocol"
	"github.com/bsv8/go-bitfs/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/bitfs"
	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/output"
	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
)

const (
	// DefaultRefundDelayBlocks is the buyer's refund lock delay when the host
	// does not override it.
	DefaultRefundDelayBlocks = 5
	// deliveryDeadlineSeconds mirrors the browser client: the authorization
	// deadline is now + 60s, clamped to the quote expiry.
	deliveryDeadlineSeconds = 60
)

// QuoteRecord is the exact seller quote plus the buyer-selected pool keys.
type QuoteRecord struct {
	QuoteRaw         []byte
	SellerPublicKey  protocol.PublicKey
	ArbiterPublicKey protocol.PublicKey
	ExpiresAtUnix    int64
}

// QuoteSource resolves the exact signed quote for a seller session.
type QuoteSource interface {
	Quote(ctx context.Context, sellerID, sessionID string) (*QuoteRecord, error)
}

// ProgressPeer optionally reports real delivery bytes while the frame arrives.
type ProgressPeer interface {
	DeliverWithProgress(ctx context.Context, poolID string, kind5 []byte, progress func(bytes int64)) ([]byte, error)
}

// Peer carries protocol artifacts to one seller session. It must only return
// verified seller artifacts; the execution layer re-verifies everything.
type Peer interface {
	// Open sends exact Kind 2 and returns exact Kind 3.
	Open(ctx context.Context, poolID string, kind2 []byte) ([]byte, error)
	// Funding sends exact Kind 4 after the funding transaction was broadcast.
	Funding(ctx context.Context, poolID string, kind4 []byte) error
	// Deliver sends exact Kind 5 and returns exact Kind 6.
	Deliver(ctx context.Context, poolID string, kind5 []byte) ([]byte, error)
	// Payment sends exact Kind 7 so the seller can advance its own state.
	Payment(ctx context.Context, poolID string, kind7 []byte) error
	// Close sends exact Kind 12 and returns exact Kind 13.
	Close(ctx context.Context, poolID string, kind12 []byte) ([]byte, error)
}

// BuyerPorts executes scheduler commands on one download task.
type BuyerPorts struct {
	Task          *bitfs.BuyerTask
	Peer          Peer
	PeerFor       func(sessionID string) (Peer, error)
	Quotes        QuoteSource
	peerMu        sync.Mutex
	peerByID      map[string]Peer
	sessionActive map[string]bool
	fundedMu      sync.Mutex
	funded        map[string]string
	// Feed delivers one scheduler fact; it may advance the kernel and
	// execute follow-up commands synchronously.
	Feed func(event scheduler.Event) error
	// Settle confirms a command once its execution fact is persisted.
	Settle func(commandID string) error
	// Now returns scheduler milliseconds; defaults to the wall clock.
	Now func() int64
	// SeedHashHex is the task's seed identity.
	SeedHashHex string
	// RefundDelayBlocks overrides DefaultRefundDelayBlocks.
	RefundDelayBlocks uint32
	// OutputDir enables automatic file assembly once every block is staged.
	OutputDir string
	// OnCommandError observes a swallowed command failure for diagnostics
	// (the scheduler only receives the protocol-level fact).
	OnCommandError func(command scheduler.Command, err error)
	// StateMu serializes local wallet/ledger/journal/store read-write against
	// other pools' commands. Network I/O runs outside the lock.
	StateMu *sync.Mutex
	// ContentBytes returns the total content bytes for a request, used to clamp
	// progress facts to the kernel's allowed range.
	ContentBytes func(ids []string) int64
}

func (p *BuyerPorts) lockState() func() {
	if p.StateMu == nil {
		return func() {}
	}
	p.StateMu.Lock()
	return p.StateMu.Unlock
}

func (p *BuyerPorts) noteError(command scheduler.Command, err error) {
	if p.OnCommandError != nil && err != nil {
		p.OnCommandError(command, err)
	}
}

func (p *BuyerPorts) validate() error {
	if p.Task == nil || (p.Peer == nil && p.PeerFor == nil) || p.Feed == nil || p.Settle == nil {
		return errors.New("host: task, peer, feed and settle are required")
	}
	if p.SeedHashHex == "" {
		return errors.New("host: seed hash is required")
	}
	return nil
}

// RestoreFunded rebuilds the funding-command association from a persisted
// kernel state before the loop replays its outbox.
func (p *BuyerPorts) RestoreFunded(state scheduler.State) {
	if p.Task == nil {
		return
	}
	for poolID, pool := range state.Pools {
		if pool.OpenCommandID == "" {
			continue
		}
		reflect := false
		switch pool.Funding {
		case "open", "closing", "closed":
			reflect = true
		case "opening":
			if p.Task.Ledger != nil {
				if record, ok := p.Task.Ledger.Pools()[poolID]; ok {
					reflect = record.State == ledger.PoolOpen || record.State == ledger.PoolRecoveryPending || record.State == ledger.PoolClosed
				}
			}
		}
		if reflect {
			p.recordFunded(poolID, pool.OpenCommandID)
		}
	}
}

func (p *BuyerPorts) recordFunded(poolID, commandID string) {
	p.fundedMu.Lock()
	defer p.fundedMu.Unlock()
	if p.funded == nil {
		p.funded = map[string]string{}
	}
	p.funded[poolID] = commandID
}

// WalletSnapshot returns the spendable balance and the open commands whose
// funding is already reflected in that same snapshot. Both values are read
// under the state lock, so an open cannot interleave between them.
func (p *BuyerPorts) WalletSnapshot(ctx context.Context) (uint64, []string, error) {
	unlock := p.lockState()
	defer unlock()
	if p.Task == nil || p.Task.Wallet == nil {
		return 0, nil, errors.New("host: wallet is required")
	}
	available, err := p.Task.Wallet.Available(ctx)
	if err != nil {
		return 0, nil, err
	}
	p.fundedMu.Lock()
	commands := make([]string, 0, len(p.funded))
	for _, command := range p.funded {
		commands = append(commands, command)
	}
	p.fundedMu.Unlock()
	sort.Strings(commands)
	return available, commands, nil
}

func (p *BuyerPorts) releaseSession(sessionID string) {
	if sessionID == "" {
		return
	}
	p.peerMu.Lock()
	delete(p.sessionActive, sessionID)
	p.peerMu.Unlock()
}

func (p *BuyerPorts) peerForSession(sessionID string) (Peer, error) {
	if p.PeerFor != nil {
		return p.PeerFor(sessionID)
	}
	if p.Peer == nil {
		return nil, errors.New("host: peer is required")
	}
	return p.Peer, nil
}

func (p *BuyerPorts) bindPeer(poolID string, peer Peer) {
	p.peerMu.Lock()
	defer p.peerMu.Unlock()
	if p.peerByID == nil {
		p.peerByID = map[string]Peer{}
	}
	p.peerByID[poolID] = peer
}

func (p *BuyerPorts) peerForPool(poolID string) (Peer, error) {
	p.peerMu.Lock()
	peer := p.peerByID[poolID]
	p.peerMu.Unlock()
	if peer != nil {
		return peer, nil
	}
	if p.Peer != nil {
		return p.Peer, nil
	}
	return nil, fmt.Errorf("host: no peer bound for pool %s", poolID)
}

func (p *BuyerPorts) now() int64 {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UnixMilli()
}

func (p *BuyerPorts) emit(eventID string, event scheduler.Event) error {
	event.EventID = eventID
	return p.Feed(event)
}

func boolPtr(value bool) *bool { return &value }

func (p *BuyerPorts) settle(commandID string) error {
	return p.Settle(commandID)
}

// OpenPool runs the full opening: exact Kind 2 -> seller Kind 3 -> funding
// broadcast. A failure before the funding transaction is handed out releases
// the intent; a failure afterwards is reported as an unknown funding result so
// the pool is never silently re-opened.
func (p *BuyerPorts) OpenPool(ctx context.Context, command scheduler.Command) error {
	if err := p.validate(); err != nil {
		return err
	}
	amount, err := positiveSats(command.Amount)
	if err != nil {
		return fmt.Errorf("host: open command amount: %w", err)
	}
	peer, err := p.peerForSession(command.SessionID)
	if err != nil {
		return p.openFailed(command, true, "peer-unavailable")
	}
	if p.PeerFor != nil {
		p.peerMu.Lock()
		if p.sessionActive == nil {
			p.sessionActive = map[string]bool{}
		}
		if p.sessionActive[command.SessionID] {
			p.peerMu.Unlock()
			// The session is already serving another pool (one quote = one pool).
			return p.openFailed(command, true, "session-busy")
		}
		p.sessionActive[command.SessionID] = true
		p.peerMu.Unlock()
	}
	if p.Quotes == nil {
		return errors.New("host: quote source is required")
	}
	record, err := p.Quotes.Quote(ctx, command.SellerID, command.SessionID)
	if err == nil {
		// Remember the seller's recommended filename for finalization.
		if verified, verifyErr := buyer.AcceptQuote(p.Task.Facts(), record.QuoteRaw); verifyErr == nil {
			unlock := p.lockState()
			if exists, _ := p.Task.Store.Exists("output/preferred-name"); !exists {
				_ = p.Task.Store.Save("output/preferred-name", []byte(verified.Terms().RecommendedFilename))
			}
			unlock()
		}
	}
	if err != nil {
		// A quote that cannot be resolved is not usable: take the seller
		// offline so the kernel stops re-opening pools from it until a fresh
		// quote arrives.
		if err := p.emit("seller-offline-"+command.SellerID, scheduler.Event{
			Type: "seller", SellerID: command.SellerID, Online: boolPtr(false),
		}); err != nil {
			return err
		}
		return p.openFailed(command, true, "quote-unavailable")
	}
	delay := p.RefundDelayBlocks
	if delay == 0 {
		delay = DefaultRefundDelayBlocks
	}
	refundLockTime, err := refundLockHeight(int64(p.Task.Facts().BlockHeight), delay)
	if err != nil {
		return p.openFailed(command, true, "refund-lock-invalid")
	}
	unlock := p.lockState()
	preparation, err := p.Task.PreparePool(ctx, bitfs.PreparePoolInput{
		PoolID:             command.PoolID,
		SellerID:           command.SellerID,
		SessionID:          command.SessionID,
		QuoteRaw:           record.QuoteRaw,
		SellerPublicKey:    record.SellerPublicKey,
		ArbiterPublicKey:   record.ArbiterPublicKey,
		PoolAmountSatoshis: amount,
		ExpiryLockTime:     refundLockTime,
	})
	unlock()
	if err != nil {
		p.releaseSession(command.SessionID)
		return p.openFailed(command, true, "prepare-failed")
	}
	p.bindPeer(command.PoolID, peer)
	kind3, err := peer.Open(ctx, command.PoolID, preparation.Kind2)
	if err != nil {
		// The funding was never broadcast: release the prepared inputs so a
		// retry can build a new funding transaction.
		p.Task.Wallet.Release(preparation.Funding)
		p.releaseSession(command.SessionID)
		return p.openFailed(command, true, "peer-failed")
	}
	unlock = p.lockState()
	completion, err := p.Task.CompletePool(ctx, preparation, kind3)
	if err == nil {
		// The funding was broadcast; bind its open command to this funding
		// boundary before releasing the lock so a wallet snapshot cannot see a
		// post-funding balance with a pre-funding reflected set.
		p.recordFunded(command.PoolID, command.CommandID)
	}
	unlock()
	if err != nil {
		// The funding transaction may already be in flight: report an unknown
		// result instead of pretending it was released.
		p.releaseSession(command.SessionID)
		return p.unknown(command, command.PoolID)
	}
	if err := peer.Funding(ctx, command.PoolID, completion.Kind4); err != nil {
		// The pool is open on chain but the seller did not acknowledge it;
		// never silently continue as if the session existed.
		return p.unknown(command, command.PoolID)
	}
	if err := p.emit("opened-"+command.PoolID, scheduler.Event{
		Type:      "opened",
		PoolID:    command.PoolID,
		CommandID: command.CommandID,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

func (p *BuyerPorts) openFailed(command scheduler.Command, released bool, reason string) error {
	if err := p.emit("open-failed-"+command.PoolID+"-"+reason, scheduler.Event{
		Type:      "open-failed",
		PoolID:    command.PoolID,
		CommandID: command.CommandID,
		Released:  &released,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

// DispatchDownload requests one content batch: exact Kind 5 -> seller Kind 6
// -> verification and local payment journal, then stages the payloads for
// file assembly. Payments are never broadcast.
//
// The port is idempotent: a batch whose rounds are already journaled only
// re-emits the delivery fact, and a payment-update send failure never turns a
// paid round into a safe-to-retry purchase.
func (p *BuyerPorts) DispatchDownload(ctx context.Context, command scheduler.Command) error {
	if err := p.validate(); err != nil {
		return err
	}
	peer, err := p.peerForPool(command.PoolID)
	if err != nil {
		return p.downloadRejected(command, err, false, "peer")
	}
	started := p.now()
	unlock := p.lockState()
	if err := bitfs.ReconcilePoolLedger(p.Task, command.PoolID); err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "ledger")
	}
	// Finish a pending round from its persisted verified bundle first: it may
	// already contain the delivered bytes and its local payment.
	if _, err := bitfs.CompletePendingRound(ctx, p.Task, command.PoolID, func(kind7 []byte) error {
		artifact, err := wire.ParseAs(wire.PaymentUpdate, kind7)
		if err != nil {
			return err
		}
		return peer.Payment(ctx, command.PoolID, artifact.Bytes())
	}); err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "pending-recovery")
	}
	purchased, err := bitfs.PurchasedHashes(p.Task.Store, command.PoolID)
	if err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "journal")
	}
	allPurchased := len(command.ContentIDs) > 0
	anyPurchased := false
	for _, id := range command.ContentIDs {
		if purchased[id] {
			anyPurchased = true
		} else {
			allPurchased = false
		}
	}
	if allPurchased {
		unlock()
		return p.emitDeliveryAndSettle(command, started)
	}
	if anyPurchased {
		unlock()
		return p.downloadRejected(command, errors.New("host: batch is only partially journaled"), false, "partial-journal")
	}
	pending, err := bitfs.PendingContentRequest(p.Task.Store, command.PoolID)
	if err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "pending")
	}
	pendingAtStart := pending != nil

	facts := p.Task.Facts()
	meta, err := bitfs.LoadPoolMeta(p.Task.Store, command.PoolID)
	if err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "pool")
	}
	quoteRaw, err := meta.QuoteRaw()
	if err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "quote")
	}
	verified, err := buyer.AcceptQuote(facts, quoteRaw)
	if err != nil {
		if pendingAtStart {
			delivered, resolveErr := bitfs.ResolveExpiredPending(p.Task, command.PoolID, "quote expired while a pending round existed")
			if resolveErr != nil {
				unlock()
				return p.downloadRejected(command, resolveErr, false, "pending-responsibility")
			}
			if delivered {
				unlock()
				return p.downloadRejected(command, err, false, "pending-responsibility")
			}
		}
		unlock()
		return p.downloadRejected(command, err, false, "quote-expired")
	}
	hashes := make([][]byte, 0, len(command.ContentIDs))
	seedOnly := true
	for _, id := range command.ContentIDs {
		hash, err := hex.DecodeString(id)
		if err != nil || len(hash) != 32 {
			unlock()
			return fmt.Errorf("host: content id %q is not a 32-byte hash", id)
		}
		hashes = append(hashes, hash)
		if id != p.SeedHashHex {
			seedOnly = false
		}
	}
	var seed []byte
	if !seedOnly {
		seed, err = p.Task.Store.Load("staging/seed")
		if err != nil {
			unlock()
			return p.downloadRejected(command, err, false, "seed")
		}
	}
	// The authorization deadline is a protocol fact in Unix seconds; the
	// scheduler's event times are monotonic milliseconds.
	nowUnix := p.Task.Facts().Now.Unix()
	deadline := nowUnix + deliveryDeadlineSeconds
	if expires := verified.Terms().QuoteExpiresAtUnixSeconds; deadline > expires {
		deadline = expires
	}
	if deadline <= nowUnix {
		safeToRetry := !pendingAtStart
		if pendingAtStart {
			delivered, resolveErr := bitfs.ResolveExpiredPending(p.Task, command.PoolID, "delivery deadline passed while a pending round existed")
			if resolveErr != nil {
				unlock()
				return p.downloadRejected(command, resolveErr, false, "pending-responsibility")
			}
			if delivered {
				unlock()
				return p.downloadRejected(command, errors.New("host: delivered pending round has no payment authorization left"), false, "pending-responsibility")
			}
			safeToRetry = true
		}
		unlock()
		return p.downloadRejected(command, errors.New("host: delivery deadline already passed"), safeToRetry, "deadline")
	}
	request, err := p.Task.PrepareContentRequest(ctx, bitfs.ContentRequestInput{
		PoolID:           command.PoolID,
		ContentHashes:    hashes,
		DeliveryDeadline: content.UnixSeconds(deadline),
		Seed:             seed,
	})
	if err != nil {
		unlock()
		return p.downloadRejected(command, err, !pendingAtStart, "prepare")
	}
	unlock()

	var kind6 []byte
	var totalBytes int64
	if p.ContentBytes != nil {
		totalBytes = p.ContentBytes(command.ContentIDs)
	}
	if progressPeer, ok := peer.(ProgressPeer); ok {
		var lastProgress int64
		kind6, err = progressPeer.DeliverWithProgress(ctx, command.PoolID, request.Kind5, func(bytes int64) {
			if totalBytes <= 0 {
				return
			}
			if bytes > totalBytes {
				bytes = totalBytes
			}
			if bytes <= lastProgress {
				return
			}
			lastProgress = bytes
			_ = p.emit(fmt.Sprintf("progress-%s-%d", command.RequestID, bytes), scheduler.Event{
				Type: "progress", RequestID: command.RequestID, Bytes: &bytes,
			})
		})
	} else {
		kind6, err = peer.Deliver(ctx, command.PoolID, request.Kind5)
	}
	if err != nil {
		// The exact Kind 5 is already persisted; the seller may have delivered.
		// This is never a safe automatic retry: recovery must first resolve the
		// pending authorization.
		return p.downloadRejected(command, err, false, "deliver")
	}

	unlock = p.lockState()
	result, err := p.Task.CompleteDelivery(ctx, bitfs.DeliveryInput{
		PoolID: command.PoolID,
		Kind5:  request.Kind5,
		Kind6:  kind6,
		Seed:   seed,
	})
	if err != nil {
		authorizationID, idErr := bitfs.AuthorizationIDOfKind5(request.Kind5)
		_, journaled, journalErr := bitfs.JournaledRound(p.Task.Store, command.PoolID, authorizationID)
		if idErr == nil && journalErr == nil && journaled {
			// The round is journaled: the local payment exists even though a
			// later step failed. Never let the kernel buy the batch again.
			unlock()
			p.noteError(command, err)
			return p.emitDeliveryAndSettle(command, started)
		}
		unlock()
		return p.downloadRejected(command, err, false, "delivery")
	}
	// CompleteDelivery stages the verified bytes before advancing the payment
	// journal, so the port only assembles once the staged content is complete.
	// A complete staged file is finalized as soon as it exists; an assembly
	// problem is diagnostic and never rolls back a verified payment round.
	if _, err := p.completeFileIfReadyLocked(ctx); err != nil {
		p.noteError(command, err)
	}
	paid, err := paidForBatch(command, command.ContentIDs)
	if err != nil {
		unlock()
		return p.downloadRejected(command, err, false, "prices")
	}
	if committed := result.CommittedSatoshis; committed != 0 && new(big.Int).SetUint64(committed).Cmp(paid) != 0 {
		// The kernel's prices and the protocol payment disagree: report the
		// unknown result instead of inventing a fact or buying again.
		unlock()
		return p.downloadRejected(command, errors.New("host: kernel price and protocol payment disagree"), false, "price-mismatch")
	}
	unlock()
	if err := peer.Payment(ctx, command.PoolID, result.Kind7); err != nil {
		// The payment update is local evidence and the round is journaled; the
		// seller state can be re-established from the saved Kind 7 on reconnect.
		p.noteError(command, err)
	}
	return p.emitDeliveryAndSettle(command, started)
}

func (p *BuyerPorts) emitDeliveryAndSettle(command scheduler.Command, started int64) error {
	paid, err := paidForBatch(command, command.ContentIDs)
	if err != nil {
		return p.downloadRejected(command, err, false, "prices")
	}
	ended := p.now()
	if started >= ended {
		started = ended - 1
	}
	if started < 0 {
		started = 0
	}
	if err := p.emit("delivery-"+command.RequestID, scheduler.Event{
		Type:      "delivery",
		RequestID: command.RequestID,
		Accepted:  append([]string(nil), command.ContentIDs...),
		Paid:      paid.String(),
		StartedAt: started,
		EndedAt:   ended,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

// downloadRejected reports a failed download round. safeToRetry must be false
// whenever an authorization, delivery or payment may already exist: marking
// such a round safe would let the kernel authorize the same content again.
func (p *BuyerPorts) downloadRejected(command scheduler.Command, cause error, safeToRetry bool, reason string) error {
	p.noteError(command, cause)
	if err := p.emit("rejected-"+command.RequestID+"-"+reason, scheduler.Event{
		Type:        "request-rejected",
		RequestID:   command.RequestID,
		SafeToRetry: &safeToRetry,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

func (p *BuyerPorts) unknown(command scheduler.Command, poolID string) error {
	if err := p.emit("unknown-"+command.CommandID, scheduler.Event{
		Type:      "unknown",
		PoolID:    poolID,
		CommandID: command.CommandID,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

// AssembleNow finalizes the download if every block is staged.
func (p *BuyerPorts) AssembleNow(ctx context.Context) (*output.Result, error) {
	return p.completeFileIfReady(ctx)
}

func (p *BuyerPorts) completeFileIfReady(ctx context.Context) (*output.Result, error) {
	unlock := p.lockState()
	defer unlock()
	return p.completeFileIfReadyLocked(ctx)
}

func (p *BuyerPorts) completeFileIfReadyLocked(ctx context.Context) (*output.Result, error) {
	if p.OutputDir == "" || p.Task == nil || p.Task.Store == nil {
		return nil, nil
	}
	preferred := p.SeedHashHex
	if raw, err := p.Task.Store.Load("output/preferred-name"); err == nil && len(raw) > 0 {
		preferred = string(raw)
	}
	assembler, err := output.New(p.Task.Store, p.OutputDir)
	if err != nil {
		return nil, err
	}
	result, err := assembler.Complete(ctx, p.SeedHashHex, preferred)
	if errors.Is(err, output.ErrIncomplete) {
		return nil, nil
	}
	return result, err
}

// DrainPool stops new dispatches for one pool; in-flight requests finish on
// their own. The kernel already marked the pool as draining when it emitted
// the command, so the intent is complete once recorded.
func (p *BuyerPorts) DrainPool(_ context.Context, command scheduler.Command) error {
	if err := p.validate(); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

// ClosePool requests exact Kind 12, verifies exact Kind 13, persists the close
// transaction and observes the seller broadcast. returned/spent are derived
// from the ledger, never from the kernel's own numbers.
func (p *BuyerPorts) ClosePool(ctx context.Context, command scheduler.Command) error {
	if err := p.validate(); err != nil {
		return err
	}
	peer, err := p.peerForPool(command.PoolID)
	if err != nil {
		return p.closeUnknown(command, err)
	}
	if poolRecord, ok := p.Task.Ledger.Pools()[command.PoolID]; ok {
		defer p.releaseSession(poolRecord.SessionID)
	}
	unlock := p.lockState()
	if err := bitfs.ReconcilePoolLedger(p.Task, command.PoolID); err != nil {
		unlock()
		return p.closeUnknown(command, err)
	}
	if poolRecord, ok := p.Task.Ledger.Pools()[command.PoolID]; ok && poolRecord.State == ledger.PoolClosed && poolRecord.RecoveryTxID != "" {
		// A previous attempt persisted the close but the closed fact did not
		// reach the kernel: re-emit it instead of sending a second Kind 12.
		unlock()
		return p.emitClosedFromLedger(command, poolRecord)
	}
	// Never close over an accepted but unjournaled round: it may already be a
	// seller delivery and an outstanding buyer obligation.
	if _, err := bitfs.CompletePendingRound(ctx, p.Task, command.PoolID, func(kind7 []byte) error {
		artifact, err := wire.ParseAs(wire.PaymentUpdate, kind7)
		if err != nil {
			return err
		}
		return peer.Payment(ctx, command.PoolID, artifact.Bytes())
	}); err != nil {
		unlock()
		return p.closeUnknown(command, err)
	}
	if pending, err := bitfs.PendingContentRequest(p.Task.Store, command.PoolID); err != nil {
		unlock()
		return p.closeUnknown(command, err)
	} else if pending != nil {
		unlock()
		return errors.New("host: refusing to close with an accepted but unjournaled round")
	}
	preparation, err := p.Task.PrepareClose(ctx, command.PoolID)
	unlock()
	if err != nil {
		return p.closeUnknown(command, err)
	}
	kind13, err := peer.Close(ctx, command.PoolID, preparation.Kind12)
	if err != nil {
		return p.closeUnknown(command, err)
	}
	unlock = p.lockState()
	completion, err := p.Task.CompleteClose(ctx, command.PoolID, kind13)
	if err != nil {
		unlock()
		return p.closeUnknown(command, err)
	}
	if !completion.Closed {
		unlock()
		return p.closeUnknown(command, err)
	}
	poolRecord, ok := p.Task.Ledger.Pools()[command.PoolID]
	if !ok {
		unlock()
		return p.closeUnknown(command, err)
	}
	parsed, err := tx.NewTransactionFromBytes(completion.Transaction)
	if err != nil {
		unlock()
		return p.closeUnknown(command, err)
	}
	if len(parsed.Inputs) != 1 {
		unlock()
		return p.closeUnknown(command, err)
	}
	var outputs uint64
	for _, output := range parsed.Outputs {
		outputs += output.Satoshis
	}
	if outputs > poolRecord.Amount {
		unlock()
		return p.closeUnknown(command, err)
	}
	spent := poolRecord.Amount - outputs
	if poolRecord.Committed > poolRecord.Amount-spent {
		unlock()
		return p.closeUnknown(command, err)
	}
	returned := poolRecord.Amount - spent - poolRecord.Committed
	unlock()
	if err := p.emit("closed-"+command.PoolID, scheduler.Event{
		Type:      "closed",
		PoolID:    command.PoolID,
		CommandID: command.CommandID,
		Returned:  new(big.Int).SetUint64(returned).String(),
		Spent:     new(big.Int).SetUint64(spent).String(),
	}); err != nil {
		return err
	}
	if err := p.emit("refund-"+command.PoolID, scheduler.Event{
		Type:   "refund-available",
		PoolID: command.PoolID,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}

func (p *BuyerPorts) closeUnknown(command scheduler.Command, cause error) error {
	p.noteError(command, cause)
	return p.unknown(command, command.PoolID)
}

// refundLockHeight derives the block-height refund lock: current height plus
// the delay, kept inside the block-height lock range.
func refundLockHeight(current int64, delayBlocks uint32) (protocol.RefundLockTime, error) {
	if current <= 0 || delayBlocks == 0 {
		return 0, errors.New("host: invalid refund lock inputs")
	}
	height := current + int64(delayBlocks)
	if height <= 0 || height >= 500_000_000 {
		return 0, errors.New("host: refund lock height is out of range")
	}
	return protocol.RefundLockTime(uint32(height)), nil
}

func positiveSats(value string) (uint64, error) {
	amount, ok := new(big.Int).SetString(value, 10)
	if !ok || amount.Sign() <= 0 {
		return 0, fmt.Errorf("invalid amount %q", value)
	}
	return amount.Uint64(), nil
}

func paidForBatch(command scheduler.Command, accepted []string) (*big.Int, error) {
	total := big.NewInt(0)
	for _, id := range accepted {
		price, ok := command.Prices[id]
		if !ok {
			return nil, fmt.Errorf("host: command has no price for content %q", id)
		}
		value, ok := new(big.Int).SetString(string(price), 10)
		if !ok || value.Sign() < 0 {
			return nil, fmt.Errorf("host: invalid price %q", price)
		}
		total.Add(total, value)
	}
	return total, nil
}

// emitClosedFromLedger rebuilds the closed/refund facts from the persisted
// ledger state, making a retried close command idempotent.
func (p *BuyerPorts) emitClosedFromLedger(command scheduler.Command, poolRecord ledger.Pool) error {
	transaction, ok := p.Task.Ledger.Transaction(poolRecord.RecoveryTxID)
	if !ok {
		return p.closeUnknown(command, errors.New("host: closed pool has no close transaction"))
	}
	raw, err := hex.DecodeString(transaction.RawHex)
	if err != nil {
		return p.closeUnknown(command, err)
	}
	parsed, err := tx.NewTransactionFromBytes(raw)
	if err != nil || len(parsed.Inputs) != 1 {
		return p.closeUnknown(command, err)
	}
	var outputs uint64
	for _, output := range parsed.Outputs {
		outputs += output.Satoshis
	}
	if outputs > poolRecord.Amount || poolRecord.Committed > poolRecord.Amount-outputs {
		return p.closeUnknown(command, errors.New("host: persisted close violates the pool ledger"))
	}
	spent := poolRecord.Amount - outputs
	returned := poolRecord.Amount - spent - poolRecord.Committed
	if err := p.emit("closed-"+command.PoolID, scheduler.Event{
		Type:      "closed",
		PoolID:    command.PoolID,
		CommandID: command.CommandID,
		Returned:  new(big.Int).SetUint64(returned).String(),
		Spent:     new(big.Int).SetUint64(spent).String(),
	}); err != nil {
		return err
	}
	if err := p.emit("refund-"+command.PoolID, scheduler.Event{
		Type:   "refund-available",
		PoolID: command.PoolID,
	}); err != nil {
		return err
	}
	return p.settle(command.CommandID)
}
