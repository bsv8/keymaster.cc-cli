// Package download drives one BitFS download through the scheduler kernel over
// one or more seller sessions. A seller session serves exactly one fee pool
// (the protocol quote is per session), so a full file uses a seed pool and one
// or more block pools, each on a fresh quote/session.
package download

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	masterseed "github.com/bsv8/MasterSeed"
	"github.com/bsv8/go-bitfs/buyer"
	"github.com/bsv8/go-bitfs/content"
	"github.com/bsv8/go-bitfs/protocol"
	"github.com/bsv8/go-bitfs/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
	"github.com/bsv8/keymaster.cc-cli/internal/bitfs"
	"github.com/bsv8/keymaster.cc-cli/internal/host"
	"github.com/bsv8/keymaster.cc-cli/internal/output"
	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/transport"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
)

const (
	// feeOpening is the wallet-funded opening miner fee estimate.
	feeOpening = "2000"
	// feeSettlement is the pool-funded close miner fee estimate.
	feeSettlement = "2000"
)

// Options configures one scheduler-driven download.
type Options struct {
	Arbitration arbitration.Resolver
	Task        *bitfs.BuyerTask
	SeedHash    string
	OutputDir   string
	Wallet      *wallet.Wallet
	Store       *store.Store
	// Accept returns the next seller session. It is called once at the start
	// and again whenever a closed pool leaves content pending. The
	// implementation is responsible for republishing the demand if needed.
	Accept func(ctx context.Context) (*transport.Session, error)
	// OnView observes the scheduler projection while waiting (optional).
	OnView func(scheduler.View)
	// Logf receives diagnostic messages (optional).
	Logf func(format string, args ...any)
	// Now supplies UTC Unix seconds; defaults to the wall clock.
	Now func() int64
	// Config overrides the scheduler limits (zero fields use defaults).
	Config scheduler.Config
	// WalletRefresh is the wallet-fact polling interval (default 5s).
	WalletRefresh time.Duration
	// Ready is called once the scheduler is running. It receives a thread-safe
	// event feed for live controls (price slider) and returns a stop function.
	Ready func(feed func(scheduler.Event) error) (stop func())
	// OnRecommendations observes the ranked top-5 seller filename votes.
	OnRecommendations func([]string)
}

type quoteSource struct {
	mu      sync.Mutex
	records map[string]*host.QuoteRecord
}

func quoteKey(sellerID, sessionID string) string { return sellerID + "|" + sessionID }

func (s *quoteSource) set(sellerID, sessionID string, record *host.QuoteRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[quoteKey(sellerID, sessionID)] = record
}

func (s *quoteSource) Quote(_ context.Context, sellerID, sessionID string) (*host.QuoteRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[quoteKey(sellerID, sessionID)]
	if record == nil {
		return nil, errors.New("download: no verified quote for this seller session")
	}
	return record, nil
}

// sessionPeer carries exact artifacts over one BitFS transport session.
type sessionPeer struct {
	session bitfs.PurchaseSession
}

func (p *sessionPeer) write(kind wire.Kind, raw []byte) error {
	artifact, err := wire.ParseAs(kind, raw)
	if err != nil {
		return err
	}
	return p.session.Write(artifact)
}

func (p *sessionPeer) expect(kind wire.Kind) ([]byte, error) {
	artifact, err := p.session.Read()
	if err != nil {
		return nil, err
	}
	if artifact.Kind() != kind {
		return nil, fmt.Errorf("download: expected Kind %d, got %d", kind, artifact.Kind())
	}
	return artifact.Bytes(), nil
}

func (p *sessionPeer) Open(_ context.Context, _ string, kind2 []byte) ([]byte, error) {
	if err := p.write(wire.RefundPresignRequest, kind2); err != nil {
		return nil, err
	}
	return p.expect(wire.RefundPresignResponse)
}

func (p *sessionPeer) Funding(_ context.Context, _ string, kind4 []byte) error {
	return p.write(wire.FundingTransactionDelivery, kind4)
}

func (p *sessionPeer) Deliver(_ context.Context, _ string, kind5 []byte) ([]byte, error) {
	if err := p.write(wire.ContentRequest, kind5); err != nil {
		return nil, err
	}
	return p.expect(wire.ContentDelivery)
}

// readProgressSession is implemented by transport.Session.
type readProgressSession interface {
	SetReadProgress(func(uint64))
	ReadBytes() uint64
}

// DeliverWithProgress reports real bytes received while the delivery frame is
// still arriving, so the kernel can distinguish a slow transfer from a stall.
func (p *sessionPeer) DeliverWithProgress(_ context.Context, _ string, kind5 []byte, progress func(int64)) ([]byte, error) {
	if err := p.write(wire.ContentRequest, kind5); err != nil {
		return nil, err
	}
	if tracker, ok := p.session.(readProgressSession); ok && progress != nil {
		baseline := tracker.ReadBytes()
		tracker.SetReadProgress(func(total uint64) {
			if total > baseline {
				progress(int64(total - baseline))
			}
		})
		defer tracker.SetReadProgress(nil)
	}
	return p.expect(wire.ContentDelivery)
}

func (p *sessionPeer) Payment(_ context.Context, _ string, kind7 []byte) error {
	return p.write(wire.PaymentUpdate, kind7)
}

func (p *sessionPeer) Close(_ context.Context, _ string, kind12 []byte) ([]byte, error) {
	if err := p.write(wire.PoolCloseRequest, kind12); err != nil {
		return nil, err
	}
	return p.expect(wire.PoolCloseResponse)
}

// Run drives the download until every content item is purchased and every pool
// is settled.
func Run(ctx context.Context, options Options) (*bitfs.PurchaseResult, error) {
	if options.Task == nil || options.Wallet == nil || options.Store == nil || options.Accept == nil || options.Arbitration == nil {
		return nil, errors.New("download: task, wallet, store, accept and arbitration resolver are required")
	}
	logf := options.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var monotonic *MonotonicClock
	now := options.Now
	if now == nil {
		monotonic = NewMonotonicClock()
		now = monotonic.Now
	}
	maxSellers := int(options.Config.BaseSeats + options.Config.FloatingSeats)
	if maxSellers < 1 {
		maxSellers = 2
	}
	var (
		peersMu    sync.Mutex
		peers      = map[string]*sessionPeer{}
		sessions   = map[string]*transport.Session{}
		sessionSet = map[*transport.Session]bool{}
		closing    bool
	)
	trackSession := func(session *transport.Session) bool {
		peersMu.Lock()
		defer peersMu.Unlock()
		if closing {
			_ = session.Close()
			return false
		}
		sessionSet[session] = true
		return true
	}
	untrackSession := func(session *transport.Session) {
		peersMu.Lock()
		delete(sessionSet, session)
		peersMu.Unlock()
	}
	closeSessions := func() {
		peersMu.Lock()
		closing = true
		open := make([]*transport.Session, 0, len(sessionSet))
		for session := range sessionSet {
			open = append(open, session)
		}
		sessionSet = map[*transport.Session]bool{}
		peersMu.Unlock()
		for _, session := range open {
			_ = session.Close()
		}
	}
	quotes := &quoteSource{records: map[string]*host.QuoteRecord{}}
	var loop *host.Loop
	var (
		versionsMu      sync.Mutex
		versions        = map[string]int64{}
		arbitrationFees = map[string]arbitration.Info{}
		quoteByID       = map[string]scheduler.Quote{}
		latest          = map[string]scheduler.Quote{}
		sellerSessions  = map[string][]string{}
		pendingSlots    int
		sessionSeq      int64
		manifestOnce    bool
		votesMu         sync.Mutex
		votes           = map[string]string{}
		contentMu       sync.Mutex
		contentBytes    = map[string]int64{}
	)
	quoteForSession := func(sessionID string) (host.Peer, error) {
		peersMu.Lock()
		defer peersMu.Unlock()
		peer := peers[sessionID]
		if peer == nil {
			return nil, fmt.Errorf("download: no session for %s", sessionID)
		}
		return peer, nil
	}
	// readSession accepts one seller session and reads its Kind 1 quote.
	readSession := func() (string, string, *content.FileQuoteTerms, error) {
		session, err := options.Accept(ctx)
		if err != nil {
			return "", "", nil, err
		}
		if !trackSession(session) {
			return "", "", nil, context.Canceled
		}
		// The quote read must not outlive cancellation: close the stream if the
		// context ends while the seller stays silent.
		readDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = session.Close()
			case <-readDone:
			}
		}()
		artifact, err := session.Read()
		close(readDone)
		if err != nil {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, fmt.Errorf("download: read quote: %w", err)
		}
		if artifact.Kind() != wire.FileQuote {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, fmt.Errorf("download: expected Kind 1, got %d", artifact.Kind())
		}
		raw := artifact.Bytes()
		verified, err := buyer.AcceptQuote(options.Task.Facts(), raw)
		if err != nil {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, fmt.Errorf("download: quote rejected: %w", err)
		}
		terms := verified.Terms()
		if hex.EncodeToString(verified.SeedHash()) != options.SeedHash {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, errors.New("download: quote is for another seed")
		}
		sellerID := hex.EncodeToString(verified.SellerPublicKey())
		sellerPublicKey, err := protocol.PublicKeyFromBytes(verified.SellerPublicKey())
		if err != nil {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, err
		}
		arbiters := verified.SupportedArbiterPublicKeys()
		if len(arbiters) == 0 {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, errors.New("download: quote offers no arbiter")
		}
		arbiterPublicKey, feeInfo, err := options.Arbitration.Select(ctx, arbiters)
		if err != nil {
			untrackSession(session)
			_ = session.Close()
			return "", "", nil, err
		}
		seq := atomic.AddInt64(&sessionSeq, 1)
		sessionID := fmt.Sprintf("%s-%s-%d", options.SeedHash[:16], sellerID[:16], seq)
		versionsMu.Lock()
		arbitrationFees[sessionID] = feeInfo
		versionsMu.Unlock()
		quotes.set(sellerID, sessionID, &host.QuoteRecord{
			QuoteRaw:         raw,
			SellerPublicKey:  sellerPublicKey,
			ArbiterPublicKey: arbiterPublicKey,
			ExpiresAtUnix:    terms.QuoteExpiresAtUnixSeconds,
		})
		votesMu.Lock()
		if terms.RecommendedFilename != "" {
			votes[sellerID] = terms.RecommendedFilename
		}
		ranked := RankRecommendations(votes)
		votesMu.Unlock()
		if options.OnRecommendations != nil {
			options.OnRecommendations(ranked)
		}
		peer := &sessionPeer{session: session}
		peersMu.Lock()
		peers[sessionID] = peer
		sessions[sessionID] = session
		peersMu.Unlock()
		return sellerID, sessionID, terms, nil
	}
	// feedQuote publishes a verified session quote to the kernel.
	feedQuote := func(sellerID, sessionID string, terms *content.FileQuoteTerms) error {
		versionsMu.Lock()
		versions[sellerID]++
		version := versions[sellerID]
		tail := scheduler.Sats("0")
		blockCount := masterseed.BlockCountForSourceSize(terms.FileSizeBytes)
		if blockCount > 0 {
			if size, err := masterseed.ExpectedBlockSize(terms.FileSizeBytes, blockCount-1); err == nil {
				if price, err := content.BlockPriceSatoshis(terms.FullBlockPriceSatoshis, size); err == nil {
					tail = new(big.Int).SetUint64(price).String()
				}
			}
		}
		// Map the protocol remaining lifetime onto the scheduler clock. The
		// protocol expiry is absolute Unix time and the scheduler clock may be
		// advanced (restart, monotonic base); copying the absolute value would
		// make a valid quote look expired in the scheduler domain.
		protocolNow := options.Task.Facts().Now.Unix()
		remainingSeconds := terms.QuoteExpiresAtUnixSeconds - protocolNow
		if remainingSeconds <= 0 {
			versionsMu.Unlock()
			return errors.New("download: quote has no remaining protocol lifetime")
		}
		quote := scheduler.Quote{
			ID:         sessionID,
			Version:    version,
			SellerID:   sellerID,
			SessionID:  sessionID,
			ExpiresAt:  now() + remainingSeconds*1000,
			BlockPrice: new(big.Int).SetUint64(terms.FullBlockPriceSatoshis).String(),
			SeedPrice:  new(big.Int).SetUint64(terms.SeedPriceSatoshis).String(),
			BlockBytes: int64(masterseed.BlockSize),
			TailPrice:  &tail,
			MaxBlocks:  int64(terms.MaxBlocksPerBatch),
			Fees: scheduler.Fees{Version: 1, Opening: feeOpening, Settlement: feeSettlement,
				ArbitrationBase: fmt.Sprint(arbitrationFees[sessionID].BaseSat), ArbitrationPerItem: fmt.Sprint(arbitrationFees[sessionID].PerItemSat)},
		}
		quoteByID[sessionID] = quote
		latest[sellerID] = quote
		versionsMu.Unlock()
		eventID := fmt.Sprintf("quote-%s-%d", sessionID, version)
		if err := loop.Feed(ctx, scheduler.Event{EventID: eventID, Type: "quote", Quote: &quote}); err != nil {
			return err
		}
		return loop.Feed(ctx, scheduler.Event{EventID: "seller-" + sellerID, Type: "seller", SellerID: sellerID, Online: boolPointer(true)})
	}
	// expireQuote stops the kernel from opening a new pool on a session that
	// just closed its pool.
	expireQuote := func(sellerID, closedSession string) {
		versionsMu.Lock()
		current, found := latest[sellerID]
		if !found || current.SessionID != closedSession {
			// A newer standby quote is already current; keep it.
			versionsMu.Unlock()
			return
		}
		versionsMu.Unlock()
		if !found {
			return
		}
		versionsMu.Lock()
		versions[sellerID]++
		current.Version = versions[sellerID]
		versionsMu.Unlock()
		current.ExpiresAt = now() - 1
		_ = loop.Feed(ctx, scheduler.Event{EventID: fmt.Sprintf("quote-expire-%s-%d", sellerID, current.Version), Type: "quote", Quote: &current})
	}
	// acceptSession integrates one new session into the seller seats.
	acceptSession := func() error {
		sellerID, sessionID, terms, err := readSession()
		if err != nil {
			return err
		}
		versionsMu.Lock()
		if len(sellerSessions[sellerID]) >= 2 {
			// One active + at most one standby per seller; more sessions only
			// replace quotes without adding a seat.
			versionsMu.Unlock()
			peersMu.Lock()
			if session := sessions[sessionID]; session != nil {
				_ = session.Close()
			}
			delete(sessions, sessionID)
			delete(peers, sessionID)
			peersMu.Unlock()
			return errors.New("download: seller already has two sessions")
		}
		sellerSessions[sellerID] = append(sellerSessions[sellerID], sessionID)
		versionsMu.Unlock()
		return feedQuote(sellerID, sessionID, terms)
	}
	// pump fills the seat cap with distinct sellers and replaces closed
	// sessions. It only calls Accept when a seat is actually needed, so the
	// demand is not republished in a hot loop.
	var wake = make(chan struct{}, 1)
	pump := func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			for {
				versionsMu.Lock()
				need := len(sellerSessions) < maxSellers || pendingSlots > 0
				duplicateCap := len(sellerSessions) >= maxSellers
				versionsMu.Unlock()
				if !need {
					break
				}
				if err := acceptSession(); err != nil {
					logf("接受卖方会话失败: %v", err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
					if duplicateCap {
						break
					}
					continue
				}
				versionsMu.Lock()
				if pendingSlots > 0 {
					pendingSlots--
				}
				versionsMu.Unlock()
			}
		}
	}
	firstSeller, firstSession, firstTerms, err := readSession()
	if err != nil {
		return nil, err
	}
	blockCount := masterseed.BlockCountForSourceSize(firstTerms.FileSizeBytes)
	seedBytes, err := masterseed.SeedSizeForBlockCount(blockCount)
	if err != nil {
		return nil, err
	}
	limits := options.Config
	if limits.MaxBlockPrice == "" || limits.MaxBlockPrice == "0" {
		limits.MaxBlockPrice = new(big.Int).SetUint64(firstTerms.FullBlockPriceSatoshis).String()
	}
	if limits.BaseSeats < 1 {
		limits.BaseSeats = 1
	}
	if limits.FloatingSeats < 1 {
		limits.FloatingSeats = 1
	}
	if limits.PoolBlocks < 1 {
		limits.PoolBlocks = 4
	}
	if limits.RequestTimeoutMs < 1 {
		limits.RequestTimeoutMs = 5000
	}
	adapter, err := host.OpenAdapter(options.Store, host.AdapterInit{
		TaskID:        options.SeedHash,
		SeedHash:      options.SeedHash,
		ManifestKnown: false,
		Clock:         now,
		Config:        limits,
		Content:       []scheduler.Content{{ID: options.SeedHash, Kind: "seed", Bytes: int64(seedBytes)}},
	})
	if err != nil {
		return nil, err
	}
	if monotonic != nil {
		if restored := adapter.State().Now; restored > 0 {
			monotonic.AdvanceTo(restored + 1)
		}
	}
	var walletRevision int64 = 1
	contentMu.Lock()
	contentBytes[options.SeedHash] = int64(seedBytes)
	contentMu.Unlock()
	ports := &host.BuyerPorts{
		Task:        options.Task,
		PeerFor:     quoteForSession,
		Quotes:      quotes,
		StateMu:     &sync.Mutex{},
		SeedHashHex: options.SeedHash,
		OutputDir:   options.OutputDir,
		Now:         now,
		ContentBytes: func(ids []string) int64 {
			contentMu.Lock()
			defer contentMu.Unlock()
			var total int64
			for _, id := range ids {
				total += contentBytes[id]
			}
			return total
		},
		OnCommandError: func(command scheduler.Command, cause error) {
			logf("调度命令 %s 失败: %v", command.Type, cause)
		},
	}
	executor, err := host.NewExecutor(ports)
	if err != nil {
		return nil, err
	}
	loop, err = host.NewLoop(adapter, executor)
	if err != nil {
		return nil, err
	}
	loop.OnError = func(err error) { logf("调度器: %v", err) }
	loop.OnCommands = func(command scheduler.Command, state scheduler.State) {
		if command.Type != "close" && command.Type != "drain" {
			return
		}
		pool := state.Pools[command.PoolID]
		if pool == nil {
			return
		}
		versionsMu.Lock()
		sessionsForSeller := sellerSessions[pool.SellerID]
		hasStandby := false
		for _, id := range sessionsForSeller {
			if id != pool.SessionID {
				hasStandby = true
			}
		}
		if command.Type == "close" {
			remaining := make([]string, 0, len(sessionsForSeller))
			for _, id := range sessionsForSeller {
				if id != pool.SessionID {
					remaining = append(remaining, id)
				}
			}
			sellerSessions[pool.SellerID] = remaining
		}
		versionsMu.Unlock()
		// Stop the kernel from opening another pool on this session before the
		// old pool has actually closed (the session serves exactly one pool).
		expireQuote(pool.SellerID, pool.SessionID)
		if !hasStandby && scheduler.ProjectView(state).Execution != "finished" {
			versionsMu.Lock()
			pendingSlots++
			versionsMu.Unlock()
			select {
			case wake <- struct{}{}:
			default:
			}
		}
	}
	loop.AfterEvent = func(event scheduler.Event, state scheduler.State) []scheduler.Event {
		followUps := make([]scheduler.Event, 0, 2)
		if !manifestOnce && !state.ManifestKnown && event.Type == "delivery" {
			for _, id := range event.Accepted {
				if id != options.SeedHash {
					continue
				}
				manifest, err := buildManifest(options.Store, options.SeedHash, firstTerms)
				if err != nil {
					logf("生成 manifest 失败: %v", err)
					break
				}
				manifestOnce = true
				contentMu.Lock()
				for _, item := range manifest {
					contentBytes[item.ID] = item.Bytes
				}
				contentMu.Unlock()
				followUps = append(followUps, scheduler.Event{EventID: "manifest-" + options.SeedHash, Type: "manifest", Content: manifest})
				break
			}
		}
		if event.Type == "closed" {
			if pool := state.Pools[event.PoolID]; pool != nil {
				peersMu.Lock()
				if session := sessions[pool.SessionID]; session != nil {
					_ = session.Close()
				}
				delete(sessions, pool.SessionID)
				delete(peers, pool.SessionID)
				peersMu.Unlock()
			}
		}
		if event.Type == "closed" || event.Type == "refund-available" {
			available, reflected, err := ports.WalletSnapshot(ctx)
			if err == nil {
				revision := atomic.AddInt64(&walletRevision, 1)
				followUps = append(followUps, scheduler.Event{
					EventID: fmt.Sprintf("wallet-%d", revision), Type: "wallet",
					Revision: &revision, Available: new(big.Int).SetUint64(available).String(),
					ReflectedCommands: reflected,
				})
			}
		}
		return followUps
	}
	ports.Feed = func(event scheduler.Event) error { return loop.Feed(ctx, event) }
	ports.Settle = loop.Settle
	previousVersion := int64(0)
	if state := adapter.State(); state.Sellers[firstSeller] != nil && state.Sellers[firstSeller].Quote != nil {
		previousVersion = state.Sellers[firstSeller].Quote.Version
	}
	restored := adapter.State().Revision > 0
	// Recover the funding-command association before the loop replays the
	// outbox, so the first wallet snapshot already reflects historical pools.
	ports.RestoreFunded(adapter.State())
	loop.Start(ctx)
	defer func() {
		closeSessions()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = loop.StopContext(stopCtx)
	}()

	available, reflected, err := ports.WalletSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	revision := atomic.AddInt64(&walletRevision, 1)
	if err := loop.Feed(ctx, scheduler.Event{EventID: "wallet-1", Type: "wallet", Revision: &revision, Available: new(big.Int).SetUint64(available).String(), ReflectedCommands: reflected}); err != nil {
		return nil, err
	}
	versionsMu.Lock()
	versions[firstSeller] = previousVersion
	sellerSessions[firstSeller] = append(sellerSessions[firstSeller], firstSession)
	versionsMu.Unlock()
	if err := feedQuote(firstSeller, firstSession, firstTerms); err != nil {
		return nil, err
	}
	intent := "start"
	if restored {
		intent = "resume"
	}
	if err := loop.Feed(ctx, scheduler.Event{EventID: intent + "-1", Type: intent}); err != nil {
		return nil, err
	}
	go pump()
	select {
	case wake <- struct{}{}:
	default:
	}
	stopControls := func() {}
	if options.Ready != nil {
		stopControls = options.Ready(func(event scheduler.Event) error { return loop.Feed(ctx, event) })
	}
	defer stopControls()
	// Keep the kernel's clock moving (request timeouts) and refresh the wallet
	// fact periodically so a top-up can unblock a zero-balance start.
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		refresh := options.WalletRefresh
		if refresh <= 0 {
			refresh = 5 * time.Second
		}
		walletTick := time.NewTicker(refresh)
		defer walletTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = loop.Feed(ctx, scheduler.Event{EventID: fmt.Sprintf("tick-%d", time.Now().UnixNano()), Type: "tick"})
			case <-walletTick.C:
				available, reflected, err := ports.WalletSnapshot(ctx)
				if err != nil {
					continue
				}
				revision := atomic.AddInt64(&walletRevision, 1)
				_ = loop.Feed(ctx, scheduler.Event{
					EventID: fmt.Sprintf("wallet-auto-%d", revision), Type: "wallet",
					Revision: &revision, Available: new(big.Int).SetUint64(available).String(),
					ReflectedCommands: reflected,
				})
			}
		}
	}()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		view, err := loop.View(ctx)
		if err != nil {
			return nil, err
		}
		if options.OnView != nil {
			options.OnView(view)
		}
		if view.Funds == "problem" {
			return nil, errors.New("download: scheduler reports a funding problem; check unresolved records")
		}
		if view.Execution != "finished" || view.Funds != "settled" {
			continue
		}
		assembler, err := output.New(options.Store, options.OutputDir)
		if err != nil {
			return nil, err
		}
		final, err := assembler.Final()
		if err != nil || final == nil {
			continue
		}
		committed := uint64(0)
		for id, pool := range options.Task.Ledger.Pools() {
			if strings.HasPrefix(id, options.SeedHash+"/") {
				committed += pool.Committed
			}
		}
		votesMu.Lock()
		recommended := RankRecommendations(votes)
		votesMu.Unlock()
		if len(recommended) > 0 && options.OutputDir != "" {
			if assembler, err := output.New(options.Store, options.OutputDir); err == nil {
				if renamed, err := assembler.RenameFinal(recommended[0]); err == nil {
					final = renamed
				}
			}
		}
		return &bitfs.PurchaseResult{PoolID: "", Path: final.Path, Bytes: final.Bytes, Committed: committed, RecommendedNames: recommended}, nil
	}
}

func buildManifest(workStore *store.Store, seedHash string, terms *content.FileQuoteTerms) ([]scheduler.Content, error) {
	seed, err := workStore.Load("staging/seed")
	if err != nil {
		return nil, err
	}
	blockCount := masterseed.BlockCountForSourceSize(terms.FileSizeBytes)
	manifest := make([]scheduler.Content, 0, blockCount)
	for index := uint64(0); index < blockCount; index++ {
		hash, err := masterseed.ReadBlockHash(context.Background(), bytes.NewReader(seed), uint64(len(seed)), index)
		if err != nil {
			return nil, err
		}
		size, err := masterseed.ExpectedBlockSize(terms.FileSizeBytes, index)
		if err != nil {
			return nil, err
		}
		manifest = append(manifest, scheduler.Content{ID: hex.EncodeToString(hash.Bytes()), Kind: "block", Bytes: int64(size)})
	}
	return manifest, nil
}

func boolPointer(value bool) *bool { return &value }

// AdjustPrice moves a manual price one step inside [minimum, maximum] without
// ever moving against the requested direction. A price outside the market range
// is not pulled into it by a down/up key.
func AdjustPrice(effective, minimum, maximum string, delta int) (string, error) {
	eff, okEff := new(big.Int).SetString(effective, 10)
	min, okMin := new(big.Int).SetString(minimum, 10)
	max, okMax := new(big.Int).SetString(maximum, 10)
	if !okEff || !okMin || !okMax || eff == nil || min == nil || max == nil {
		return effective, errors.New("download: invalid price value")
	}
	if delta == 0 {
		return effective, nil
	}
	span := new(big.Int).Sub(max, min)
	step := new(big.Int).Div(span, big.NewInt(20))
	if step.Sign() <= 0 {
		step = big.NewInt(1)
	}
	if delta > 0 {
		if eff.Cmp(max) >= 0 {
			return effective, nil
		}
		next := new(big.Int).Add(eff, step)
		if next.Cmp(max) > 0 {
			next = max
		}
		if next.Cmp(eff) < 0 {
			return effective, nil
		}
		return next.String(), nil
	}
	if eff.Cmp(min) <= 0 {
		return effective, nil
	}
	next := new(big.Int).Sub(eff, step)
	if next.Cmp(min) < 0 {
		next = min
	}
	if next.Cmp(eff) > 0 {
		return effective, nil
	}
	return next.String(), nil
}

// RankRecommendations counts one vote per seller (the latest recommendation of
// each seller replaces its previous vote) and returns the top five names by
// vote count, ties broken by name.
func RankRecommendations(votes map[string]string) []string {
	counts := map[string]int{}
	for _, name := range votes {
		if name == "" {
			continue
		}
		counts[name]++
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) > 5 {
		names = names[:5]
	}
	return names
}

// MonotonicClock supplies scheduler milliseconds from a monotonic elapsed
// clock. The base is wall-derived once, so a wall-clock rollback cannot move
// the kernel backwards; a restored state can advance it explicitly.
type MonotonicClock struct {
	mu    sync.Mutex
	base  int64
	start time.Time
	last  int64
}

// NewMonotonicClock starts at the current wall time.
func NewMonotonicClock() *MonotonicClock {
	now := time.Now()
	return &MonotonicClock{base: now.UnixMilli(), start: now}
}

// Now returns the current scheduler milliseconds, never decreasing.
func (c *MonotonicClock) Now() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	value := c.base + time.Since(c.start).Milliseconds()
	if value < c.last {
		value = c.last
	}
	c.last = value
	return value
}

// AdvanceTo moves the clock forward to at least target.
func (c *MonotonicClock) AdvanceTo(target int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current := c.base + time.Since(c.start).Milliseconds()
	if target > current {
		c.base += target - current
	}
	if target > c.last {
		c.last = target
	}
}
