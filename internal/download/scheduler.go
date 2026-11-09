// Package download drives one BitFS download through the scheduler kernel over
// one or more seller sessions. A seller session serves exactly one fee pool
// (the protocol quote is per session), so a full file uses a seed pool and one
// or more block pools, each on a fresh quote/session.
package download

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv8/BitFSProtocol/buyer"
	"github.com/bsv8/BitFSProtocol/content"
	"github.com/bsv8/BitFSProtocol/protocol"
	"github.com/bsv8/BitFSProtocol/wire"

	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
	"github.com/bsv8/keymaster.cc-cli/internal/bitfs"
	"github.com/bsv8/keymaster.cc-cli/internal/host"
	"github.com/bsv8/keymaster.cc-cli/internal/lock"
	"github.com/bsv8/keymaster.cc-cli/internal/output"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/transport"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
)

var errAcceptSource = errors.New("download: quote connection source unavailable")

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
	// Accept 只等待下一条真实入站连接，不自行重发需求或按席位过滤报价。
	Accept func(ctx context.Context) (*transport.Session, error)
	// RequestQuotes 严格执行内核 discover；首次用户查询由调用者显式发起。
	RequestQuotes func(context.Context) error
	// OnView observes the scheduler projection while waiting (optional).
	OnView func(scheduler.View)
	// Logf receives diagnostic messages (optional).
	Logf func(format string, args ...any)
	// Now supplies UTC Unix seconds; defaults to the wall clock.
	Now func() int64
	// Config overrides the scheduler limits (zero fields use defaults).
	Config scheduler.Config
	// Ready is called once the scheduler is running. It receives a thread-safe
	// event feed for live controls (price slider) and returns a stop function.
	Ready func(feed func(scheduler.Event) error) (stop func())
	// OnRecommendations observes the ranked top-5 seller filename votes.
	OnRecommendations func([]string)
	// PinExecID binds this run to one execution. After the seed lock is held
	// and the pointer is resolved, the run refuses with an explicit error
	// when the pointer names another execution (or none) instead of quietly
	// driving the wrong execution. Empty means interactive resume semantics.
	PinExecID string
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
	// One live download per seed in this wallet/network/store scope: a second
	// concurrent run for the same seed must manage the original task, not
	// create a second one. The lock is held for the whole run; a crash
	// releases it implicitly and the next run resumes the persisted snapshot.
	scopeLock, err := lock.Acquire(filepath.Join(options.Store.Root(), "scheduler", ".active.lock"))
	if err != nil {
		return nil, fmt.Errorf("download: seed %s already has an active download in this store: %w", options.SeedHash, err)
	}
	defer scopeLock.Release()
	// Pinned runs (admin-started background jobs) resolved their target
	// before releasing the admin seed guard; re-verify under this run's own
	// guard before blocking on seller sessions, so a pointer move in between
	// fails fast with an explicit refusal instead of driving (or waiting on)
	// the wrong execution.
	if options.PinExecID != "" {
		current, exists, pinErr := host.CurrentExecID(options.Store, options.SeedHash)
		if pinErr != nil {
			return nil, pinErr
		}
		if !exists || current != options.PinExecID {
			return nil, fmt.Errorf("download: pinned execution %s is not current (%q); refusing to drive the wrong execution", options.PinExecID, current)
		}
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
	var (
		peersMu    sync.Mutex
		peers      = map[string]*sessionPeer{}
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
		sessionSeq      int64
		manifestOnce    bool
		votesMu         sync.Mutex
		votes           = map[string]string{}
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
			return "", "", nil, errors.Join(errAcceptSource, err)
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
		peersMu.Unlock()
		return sellerID, sessionID, terms, nil
	}
	// feedQuote publishes a verified session quote to the kernel. The tail
	// price is the seller-signed quote value, used directly without derivation.
	feedQuote := func(sellerID, sessionID string, terms *content.FileQuoteTerms) error {
		versionsMu.Lock()
		versions[sellerID]++
		version := versions[sellerID]
		tail := scheduler.Sats(new(big.Int).SetUint64(terms.TailPriceSatoshis).String())
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
			TailPrice:  &tail,
			MaxBlocks:  int64(terms.MaxBlocksPerBatch),
			Fees: scheduler.Fees{Version: 1, Opening: feeOpening, Settlement: feeSettlement,
				ArbitrationBase: fmt.Sprint(arbitrationFees[sessionID].BaseSat), ArbitrationPerItem: fmt.Sprint(arbitrationFees[sessionID].PerItemSat)},
		}
		versionsMu.Unlock()
		eventID := fmt.Sprintf("quote-%s-%d", sessionID, version)
		if err := loop.Feed(ctx, scheduler.Event{EventID: eventID, Type: "quote", Quote: &quote}); err != nil {
			return err
		}
		return loop.Feed(ctx, scheduler.Event{EventID: "seller-" + sellerID, Type: "seller", SellerID: sellerID, Online: boolPointer(true)})
	}
	// 连接接收器只观察真实新报价；席位、换人和是否重发需求均由内核决定。
	intake := func() {
		for ctx.Err() == nil {
			sellerID, sessionID, terms, err := readSession()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logf("接受卖方会话失败: %v", err)
				if errors.Is(err, errAcceptSource) {
					_ = loop.Feed(ctx, scheduler.Event{EventID: "quote-source-offline", Type: "quote-source", Enabled: boolPointer(false)})
					return
				}
				continue
			}
			if err := feedQuote(sellerID, sessionID, terms); err != nil {
				logf("报价事实回灌失败: %v", err)
			}
		}
	}
	firstSeller, firstSession, firstTerms, err := readSession()
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
	sched, err := sharedScheduler(limits)
	if err != nil {
		return nil, err
	}
	create := scheduler.CreateTaskInput{SeedHash: options.SeedHash}
	// 本地重验通过才复用；种子缺失/损坏（已安全隔离）按新购买处理。
	// 隔离或存储操作失败则向上返回错误：损坏文件可能仍占据原路径，
	// 继续采购只会在交付落盘时失败，不能静默开始。
	info, ok, verifyErr := verifyLocalFile(options.Store, options.SeedHash)
	if verifyErr != nil {
		return nil, verifyErr
	}
	if ok {
		create.Resume = &scheduler.CreateResume{
			BlockHashes:       info.BlockHashes,
			ReusedBlockHashes: info.ReusedBlockHashes,
		}
	}
	// Execution resolution: normal start/resume never creates a new execution.
	// The pointer names the current execution; a missing pointer means the
	// first execution. Redownload (Admin Web) creates e2, e3, ... and switches
	// the pointer; the next Run picks the new execution up. A pinned run
	// (admin-started background job) resolved its target before releasing the
	// seed guard, so it must re-verify under this run's own guard and refuse
	// when the pointer moved instead of driving the wrong execution.
	execID, execExists, err := host.CurrentExecID(options.Store, options.SeedHash)
	if err != nil {
		return nil, err
	}
	if options.PinExecID != "" {
		if !execExists {
			return nil, fmt.Errorf("download: pinned execution %s has no pointer; refusing to drive an unknown execution", options.PinExecID)
		}
		if execID != options.PinExecID {
			return nil, fmt.Errorf("download: current execution changed from %s to %s; refusing to drive the wrong execution", options.PinExecID, execID)
		}
	} else if !execExists {
		execID, err = host.CreateExecution(options.Store, sched, options.SeedHash, create)
		if err != nil {
			return nil, err
		}
		if err := host.SwitchCurrent(options.Store, options.SeedHash, execID); err != nil {
			return nil, err
		}
	}
	adapter, err := host.OpenAdapter(options.Store, host.AdapterInit{
		Scheduler: sched,
		Create:    create,
		ExecID:    execID,
		Clock:     now,
	})
	if err != nil {
		return nil, err
	}
	if monotonic != nil {
		if restored := adapter.State().Now; restored > 0 {
			monotonic.AdvanceTo(restored + 1)
		}
	}
	ports := &host.BuyerPorts{
		Task:        options.Task,
		PeerFor:     quoteForSession,
		Quotes:      quotes,
		Discover:    options.RequestQuotes,
		StateMu:     &sync.Mutex{},
		SeedHashHex: options.SeedHash,
		ExecID:      execID,
		OutputDir:   options.OutputDir,
		Now:         now,
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
	loop.AfterEvent = func(event scheduler.Event, state scheduler.State) []scheduler.Event {
		followUps := make([]scheduler.Event, 0, 2)
		if !manifestOnce && state.Manifest == nil && event.Type == "delivery" {
			for _, id := range event.Accepted {
				if id != options.SeedHash {
					continue
				}
				manifest, err := buildManifest(options.Store, options.SeedHash)
				if err != nil {
					logf("生成 manifest 失败: %v", err)
					break
				}
				manifestOnce = true
				followUps = append(followUps, scheduler.Event{
					EventID:           "manifest-" + options.SeedHash,
					Type:              "manifest",
					BlockHashes:       manifest.hashes,
					ReusedBlockHashes: manifest.reused,
				})
				break
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
	loop.Start(ctx)
	defer func() {
		closeSessions()
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = loop.StopContext(stopCtx)
	}()

	if options.RequestQuotes != nil {
		if err := loop.Feed(ctx, scheduler.Event{EventID: "quote-source-ready", Type: "quote-source", Enabled: boolPointer(true)}); err != nil {
			return nil, err
		}
	}
	versionsMu.Lock()
	versions[firstSeller] = previousVersion
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
	go intake()
	stopControls := func() {}
	if options.Ready != nil {
		stopControls = options.Ready(func(event scheduler.Event) error { return loop.Feed(ctx, event) })
	}
	defer stopControls()
	// Keep request timeout clocks moving; funding is requested only on demand.
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = loop.Feed(ctx, scheduler.Event{EventID: fmt.Sprintf("tick-%d", time.Now().UnixNano()), Type: "tick"})
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
		if view.Failure != nil && view.Funds != "pending" {
			// 内核已经停止新工作。等待真实在途购买结束，而不是把任务错误
			// 当成取消传输；未决资金仍由原证据继续追溯。
			state, stateErr := loop.State(ctx)
			if stateErr != nil {
				return nil, stateErr
			}
			pending := false
			for _, request := range state.Requests {
				if request.State == "pending" {
					pending = true
					break
				}
			}
			if !pending {
				return nil, fmt.Errorf("download: 资金申请不足：本次需求 %s sats；任务已停止新增购买，内容 %d/%d，资金状态 %s", view.Failure.Required, view.Completed, view.Total, view.Funds)
			}
		}
		if view.Funds == "problem" && view.Failure == nil {
			return nil, errors.New("download: scheduler reports a funding problem; check unresolved records")
		}
		if view.Execution != "finished" || (view.Funds != "settled" && view.Funds != "none") {
			continue
		}
		assembler, err := output.New(options.Store, options.OutputDir)
		if err != nil {
			return nil, err
		}
		final, err := assembler.Final()
		if err != nil {
			return nil, err
		}
		if final == nil {
			// No pool was ever opened (full local reuse): every block is
			// already staged but no final file exists yet. Assemble from
			// staging instead of waiting for a pool that will never come.
			votesMu.Lock()
			recommended := RankRecommendations(votes)
			votesMu.Unlock()
			preferred := ""
			if len(recommended) > 0 {
				preferred = recommended[0]
			}
			completed, err := assembler.Complete(ctx, options.SeedHash, preferred)
			if err != nil {
				return nil, err
			}
			final = &output.FinalFile{SeedHashHex: options.SeedHash, Path: completed.Path, Bytes: completed.Bytes}
		}
		committed := uint64(0)
		for id, pool := range options.Task.Ledger.Pools() {
			if strings.HasPrefix(id, host.ExecPoolPrefix(execID, options.SeedHash)) {
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

type manifestInfo struct {
	hashes []string
	reused []string
}

// buildManifest 从本地已验证的 Seed 读出完整有序清单（含重复位置），并重验
// 本地块作为 reusedBlockHashes 提供。种子缺失或损坏时返回错误。
func buildManifest(workStore *store.Store, seedHash string) (*manifestInfo, error) {
	info, ok, err := verifyLocalFile(workStore, seedHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("本地 Seed 未通过验证")
	}
	return &manifestInfo{
		hashes: info.BlockHashes,
		reused: info.ReusedBlockHashes,
	}, nil
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
// vote count, ties broken by name. Names are sanitized before counting so
// different raw spellings of the same safe name add up; empty results are
// dropped and the caller falls back to the seed hash.
func RankRecommendations(votes map[string]string) []string {
	counts := map[string]int{}
	for _, name := range votes {
		safe := sanitizeForVote(name)
		if safe == "" {
			continue
		}
		counts[safe]++
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

// sanitizeForVote applies the same safe-name rules as final landing so that
// different raw spellings of one safe name accumulate. Empty means no valid
// name (caller falls back to the seed hash).
func sanitizeForVote(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	safe := output.SanitizeFilename(name, "")
	// SanitizeFilename returns the fallback ("") for invalid names.
	return safe
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
