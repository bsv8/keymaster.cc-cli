package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	primitives "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv8/BitFSProtocol/protocol"
	channels "github.com/bsv8/ChannelProtocol"
	"github.com/bsv8/ChannelProtocol/hashrequest"
	"github.com/bsv8/bitcoin-libp2p/golibp2p"
	"github.com/bsv8/bitcoin-libp2p/signer"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
	"github.com/bsv8/keymaster.cc-cli/internal/bitfs"
	"github.com/bsv8/keymaster.cc-cli/internal/discovery"
	"github.com/bsv8/keymaster.cc-cli/internal/download"
	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/output"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/transport"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
)

func (d *Daemon) runTask(seed string) {
	ctx, cancel := context.WithCancel(context.Background())
	d.tasks.mu.Lock()
	t, ok := d.tasks.tasks[seed]
	if !ok {
		d.tasks.mu.Unlock()
		cancel()
		return
	}
	// A cancel that landed while the task was still pending wins: never flip
	// a cancelled (or already terminal) task back to running. A shutdown in
	// progress also refuses new runs; the task stays pending for the next
	// start to resume instead of being cancelled by a stop it never saw.
	if t.State != TaskPending || d.tasks.stopping {
		d.tasks.mu.Unlock()
		cancel()
		return
	}
	t.cancel = cancel
	t.State = TaskRunning
	t.Detail = "调度器下载中"
	d.tasks.publishLocked(t)
	d.tasks.mu.Unlock()

	if err := d.executeDownload(ctx, seed); err != nil {
		if ctx.Err() != nil {
			d.tasks.mu.Lock()
			if cur, ok := d.tasks.tasks[seed]; ok && cur.State == TaskRunning {
				if d.tasks.stopping {
					// Shutdown, not a user cancel: keep the task resumable.
					// It stays pending so the next start auto-resumes it
					// against the same bound root.
					cur.State = TaskPending
					cur.Detail = "后台停止，已排队等待重启后自动续接"
				} else {
					cur.State = TaskCancelled
					cur.Detail = "已取消"
				}
				d.tasks.publishLocked(cur)
			}
			d.tasks.mu.Unlock()
			return
		}
		d.tasks.mu.Lock()
		if cur, ok := d.tasks.tasks[seed]; ok {
			cur.State = TaskFailed
			cur.Error = err.Error()
			cur.Detail = "下载失败"
			d.tasks.publishLocked(cur)
		}
		d.tasks.mu.Unlock()
		return
	}
	// A finished download is new content in its target root: refresh it into
	// the supply index (same ordinary check) and queue it to collectors.
	d.tasks.mu.Lock()
	rootID := ""
	if cur, ok := d.tasks.tasks[seed]; ok {
		rootID = cur.RootID
	}
	d.tasks.mu.Unlock()
	if rootID != "" {
		rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		d.roots.refreshOne(rctx, rootID)
		cancel()
		d.feedCollectors()
	}
}

func (d *Daemon) executeDownload(ctx context.Context, seed string) error {
	d.tasks.mu.Lock()
	t, ok := d.tasks.tasks[seed]
	if !ok {
		d.tasks.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	rootID := t.RootID
	d.tasks.mu.Unlock()

	info, settings, err := d.store.Snapshot()
	if err != nil {
		return err
	}
	var rootPath string
	for _, r := range info.Roots {
		if r.ID == rootID {
			rootPath = r.Path
			break
		}
	}
	if rootPath == "" {
		return fmt.Errorf("目标下载根不存在")
	}
	// Private key copy for this run.
	d.mu.Lock()
	if d.state != StateUnlocked || len(d.privateKey) == 0 {
		d.mu.Unlock()
		return fmt.Errorf("后台已锁定")
	}
	privateKey := append([]byte(nil), d.privateKey...)
	d.mu.Unlock()
	defer clearBytes(privateKey)

	// Single daemon per data dir (dlock) plus one live task per seed per
	// daemon (taskManager) already serialize runs. The per-seed store lock
	// below is owned by download.Run for the whole run; taking it here too
	// would self-conflict inside this process.

	clientRoot := filepath.Join(d.layout.ClientDir, seed)
	wocClient := woc.New(wocBaseURL(info.BsvNetwork), settings.WOCAPIKey)
	initialHeight, err := wocClient.ChainHeight(ctx)
	if err != nil {
		return fmt.Errorf("读取链高度失败: %w", err)
	}
	// Shared tip for the life of this task so long downloads do not settle
	// protocol facts on a stale height.
	tip := &chainHeight{}
	tip.set(initialHeight)
	go tip.refreshLoop(ctx, wocClient)
	w, releaseWallet, err := wallet.Shared(wocClient, wallet.Options{
		Network: info.BsvNetwork, PrivateKey: privateKey,
		FeeRateSatoshisPerKb: settings.FeeRateSatoshisPerKb,
	})
	if err != nil {
		return err
	}
	defer releaseWallet()
	workStore, err := store.Open(clientRoot)
	if err != nil {
		return err
	}
	l, err := ledger.Open(filepath.Join(clientRoot, "ledger.json"), seed, info.BsvNetwork, seed)
	if err != nil {
		return err
	}
	key, _ := primitives.PrivateKeyFromBytes(privateKey)
	signerInstance, err := protocol.NewPrivateKeySigner(key)
	if err != nil {
		return err
	}
	facts := func() protocol.Facts {
		return protocol.Facts{Now: time.Now(), BlockHeight: protocol.BlockHeight(tip.get())}
	}
	// Snapshot the fee policy with the task: later settings edits must not
	// change this run's funding through the shared pool.
	taskFeeRate := settings.FeeRateSatoshisPerKb
	buyerTask := &bitfs.BuyerTask{
		Wallet: w, Ledger: l, Store: workStore, Signer: signerInstance,
		Facts: facts, MinerFeeRate: protocol.SatoshisPerKilobyte(taskFeeRate),
		WalletFeeRateSatoshisPerKb: taskFeeRate,
		Chain:                      wocClient, UnresolvedDir: filepath.Join(d.layout.ClientDir, "unresolved"),
	}
	// Fast path: already finalized.
	if assembler, err := output.New(workStore, rootPath); err == nil {
		if final, err := assembler.Final(); err == nil && final != nil {
			d.tasks.mu.Lock()
			if cur, ok := d.tasks.tasks[seed]; ok {
				cur.State = TaskDone
				cur.FinalPath = final.Path
				cur.Bytes = final.Bytes
				cur.Detail = "已完成"
				d.tasks.publishLocked(cur)
			}
			d.tasks.mu.Unlock()
			return nil
		}
	}
	// Resume path: existing pool evidence.
	if pools := l.Pools(); len(pools) > 0 {
		d.updateTask(seed, func(cur *Task) {
			cur.Detail = "续接上次未完成的采购"
		})
	}
	localSigner, err := signer.NewLocalSigner(privateKey)
	if err != nil {
		return err
	}
	listenAddr, err := ma.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	if err != nil {
		return err
	}
	services, err := loadRuntimeServices(info, settings)
	if err != nil {
		return err
	}
	buyerHost, err := golibp2p.NewHost(golibp2p.HostConfig{
		Signer:           localSigner,
		ListenAddrs:      []ma.Multiaddr{listenAddr},
		TransportOptions: []libp2p.Option{libp2p.Transport(websocket.New)},
	})
	if err != nil {
		return fmt.Errorf("创建监听失败: %w", err)
	}
	defer buyerHost.Close()
	// NOTE: websocket transport is added by default in golibp2p; the explicit
	// webrtc factory above keeps browser-buyer compatibility for sellers. For
	// downloads we only need outbound dials plus inbound BitFS sessions.
	addresses := buyerHost.Addrs()
	if len(addresses) == 0 {
		return fmt.Errorf("没有可用监听地址")
	}
	locator, err := hashrequest.NewMultiaddrLocator(addresses[0].Encapsulate(ma.StringCast("/p2p/" + buyerHost.ID().String())).String())
	if err != nil {
		return err
	}
	arbitrationClient := &arbitration.Client{Host: buyerHost, Endpoints: services.ArbitrationServices}
	var supplier *appconfig.Supplier
	for i := range services.SatSuppliers {
		if services.SatSuppliers[i].Enabled {
			supplier = &services.SatSuppliers[i]
			break
		}
	}
	if supplier == nil || len(supplier.Multiaddrs) == 0 {
		return fmt.Errorf("下载需要至少一个启用的供应商")
	}
	sspClient, err := dialSSP(ctx, supplier.Multiaddrs[0], localSigner)
	if err != nil {
		return fmt.Errorf("连接供应商失败: %w", err)
	}
	defer sspClient.Close()
	channelKey, err := channels.ParsePrivateKey(privateKey)
	if err != nil {
		return err
	}
	channelPublic, err := channels.PublicKeyFromPrivate(channelKey)
	if err != nil {
		return err
	}
	publisher := &discovery.Publisher{
		Publish:    sspPublish(sspClient),
		PrivateKey: channelKey,
		PublicKey:  channelPublic,
		Locators:   []hashrequest.Locator{locator},
	}
	sessionCh := make(chan *transport.Session, 8)
	listenErr := transport.Listen(buyerHost, func(session *transport.Session) {
		select {
		case sessionCh <- session:
		default:
			_ = session.Close()
		}
	})
	if listenErr != nil {
		return fmt.Errorf("监听 BitFS 会话失败: %w", listenErr)
	}
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	// Keep the daemon cancel wired to the run context.
	go func() {
		<-ctx.Done()
		runCancel()
	}()
	if _, err := publisher.PublishDemand(runCtx, seed, 10*time.Minute); err != nil {
		return fmt.Errorf("发布需求失败: %w", err)
	}
	d.updateTask(seed, func(cur *Task) {
		cur.Detail = "已发布需求，等待卖方连接"
	})
	result, runErr := download.Run(runCtx, download.Options{
		Arbitration: arbitrationClient,
		Task:        buyerTask, SeedHash: seed, OutputDir: rootPath,
		Wallet: w, Store: workStore,
		RequestQuotes: func(requestCtx context.Context) error {
			_, err := publisher.PublishDemand(requestCtx, seed, 10*time.Minute)
			return err
		},
		Accept: func(acceptCtx context.Context) (*transport.Session, error) {
			select {
			case session := <-sessionCh:
				return session, nil
			case <-acceptCtx.Done():
				return nil, acceptCtx.Err()
			}
		},
		Logf: func(format string, args ...any) {},
		OnView: func(view scheduler.View) {
			d.updateTask(seed, func(cur *Task) {
				cur.BlocksDone = uint64(view.Completed)
				cur.BlocksTotal = uint64(view.Total)
				cur.Detail = "调度器: " + view.Execution
			})
		},
		OnRecommendations: func(names []string) {
			d.updateTask(seed, func(cur *Task) {
				cur.Recommended = append([]string(nil), names...)
			})
		},
		Config: scheduler.Config{
			MaxBlockPrice:    settings.Scheduler.MaxBlockPriceSatoshis,
			BaseSeats:        int64(settings.Scheduler.TotalSeats - settings.Scheduler.FloatingSeats),
			FloatingSeats:    int64(settings.Scheduler.FloatingSeats),
			PoolBlocks:       int64(settings.Scheduler.PoolBlocks),
			RequestTimeoutMs: int64(max(1, settings.Scheduler.RequestTimeoutMs)),
		},
	})
	if runErr != nil {
		// Surface the scheduler failure with its evidence preserved.
		return fmt.Errorf("下载失败: %w", runErr)
	}
	d.tasks.mu.Lock()
	if cur, ok := d.tasks.tasks[seed]; ok {
		cur.State = TaskDone
		cur.FinalPath = result.Path
		cur.Bytes = result.Bytes
		cur.PaidSats = fmt.Sprint(result.Committed)
		cur.Recommended = append([]string(nil), result.RecommendedNames...)
		cur.Detail = "已完成"
		d.tasks.publishLocked(cur)
	}
	d.tasks.mu.Unlock()
	_ = strings.TrimSpace
	return nil
}

func (d *Daemon) updateTask(seed string, fn func(*Task)) {
	d.tasks.mu.Lock()
	defer d.tasks.mu.Unlock()
	if cur, ok := d.tasks.tasks[seed]; ok {
		fn(cur)
		d.tasks.publishLocked(cur)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
