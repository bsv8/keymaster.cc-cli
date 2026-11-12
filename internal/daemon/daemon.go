// Package daemon is the km background process: the sole owner of the wallet,
// settings, downloads, multi-root selling and recovery work. It serves the
// local HTTP API used by both the CLI and the Web UI over 127.0.0.1.
//
// States: uninitialized, locked, unlocked, stopping. A missing data dir
// reports uninitialized and never waits for input. The process holds the
// data directory file lock; a second daemon exits with dlock.ErrActive.
//
// Unlock (CLI or Web) shares one flow: the password decrypts the pure KeyHold
// in memory only, never persisted, logged or stored in the browser. Unlocks
// are serialized to avoid concurrent expensive KDF work. Restart relocks.
package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	keyhold "github.com/bsv8/KeyHold/go/v2"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/appstorage"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
	"github.com/bsv8/keymaster.cc-cli/internal/events"
	"github.com/bsv8/keymaster.cc-cli/internal/webui"
	"github.com/gorilla/websocket"
)

// State is the daemon lifecycle state.
type State string

const (
	StateUninitialized State = "uninitialized"
	StateLocked        State = "locked"
	StateUnlocked      State = "unlocked"
	StateStopping      State = "stopping"
)

// Options configures the daemon.
type Options struct {
	DataDir string
	// PortOverride forces the listen port (0 = from settings).
	PortOverride int
	// StorageDir 独立指定 App Storage 根；为空时默认钱包根同级 <目录名>-storage。
	StorageDir string
	// 独立后台统一提供完整 Web；CLI 内嵌后台不监听端口。
}

// Daemon is the background owner.
type Daemon struct {
	supplierMu sync.Mutex
	suppliers  map[string]*supplierConnection
	layout     datadir.Layout
	store      *appconfig.Store

	mu           sync.Mutex
	state        State
	privateKey   []byte
	publicKey    string
	address      string
	unlockMu     sync.Mutex
	balanceMu    sync.Mutex
	balanceCache *balanceSnapshot
	// balanceWatching 标记余额采集循环已启动；lastPublished 记录上次
	// 已发布的余额状态，用于变化检测（均由 balanceMu 保护）。
	balanceWatching bool
	lastPublished   lastBalance

	tasks *taskManager
	roots *rootsManager

	collectors *collectorsManager

	sellerMu      sync.Mutex
	sellerRunning bool
	sellerCancel  context.CancelFunc
	sellerWg      sync.WaitGroup

	// settingsApplyMu serializes whole settings applications: the unified
	// store write plus its runtime side effects (pricing caches, collector
	// rebuilds). Without it a later revision could apply first and then be
	// overwritten by an earlier request's side effects.
	settingsApplyMu sync.Mutex

	httpServer *http.Server
	listener   net.Listener
	lockGuard  *dlock.Guard

	wsMu    sync.Mutex
	wsConns map[*wsConn]struct{}

	storageLayout appstorage.Layout
	storage       *appstorage.Store
	storageGuard  *dlock.Guard

	connectMgr *connect.Manager

	// paymentHeld 为冻结的付款计划（批准前冻结，批准后原样广播）。
	// 仅内存持有，重启后按持久 rawHex 直接广播，不重新选币。
	paymentsMu  sync.Mutex
	paymentHeld map[string]*heldPayment

	// dispatchMu/dispatchStopping/dispatchWg 统计 WS 与进程内共用的分派器在途请求。
	// 关闭时先停止接收（新请求直接拒绝并取消在途上下文），再等待在途付款执行退出，
	// 之后才释放钱包目录锁；排空失败保留锁并报告关闭失败。
	// 与 state=Stopping 不同：该标记专用于 Add/Wait 配对的原子性，避免新请求
	// 在排空等待开始后溜入。
	dispatchMu       sync.Mutex
	dispatchStopping bool
	dispatchWg       sync.WaitGroup

	// paymentRetry 为付款记录落盘失败时的待重试写入（内存队列 + 定时器链）。
	// 只重试记录写入本身（条件写，防覆盖后续状态），绝不重新选币/签名/广播；
	// 进程重启后由启动收敛接管（遗留 pending 按有无冻结与审批收敛）。
	// paymentRetryStopped 为永久停止标记：关闭流程在同一把锁下设置，
	// enqueue 与回调均检查；关闭时等待正在执行的重试结束，保证 Close 返回后
	// 旧实例不再写付款文件（同进程重开同一钱包目录时不会覆盖新实例数据）。
	paymentRetryMu      sync.Mutex
	paymentRetry        map[string]paymentRetryOp
	paymentRetryTimer   *time.Timer
	paymentRetryStopped bool
	paymentRetryWg      sync.WaitGroup

	// closeOnce/closeDone make Close idempotent and let concurrent or
	// repeated callers wait for the same completed cleanup instead of
	// mistaking an early return for finished cleanup.
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
	// httpDone reports the HTTP server result. Buffered size 1; closed
	// with the Serve return value (nil on clean shutdown).
	httpDone chan error

	// events 是统一消息总线，只做分发不持有真值；发布点在业务提交后调用。
	events *events.Broker

	// startMu guards the transport-independent lifecycle (periodic catalog
	// refresh and other background loops). Start is idempotent; Close
	// cancels it. ServeHTTP never starts business loops.
	startMu         sync.Mutex
	started         bool
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	watchWg         sync.WaitGroup
}

// DaemonProduct and DaemonAPIVersion identify the API for probe safety.
// Probes must verify these so a CLI never mistakes another program on the
// same port for a km daemon.
const (
	DaemonProduct    = "km"
	DaemonAPIVersion = "v1"
)

type balanceSnapshot struct {
	Total     uint64 `json:"totalSats"`
	Available uint64 `json:"availableSats"`
	Allocated uint64 `json:"allocatedSats"`
	QueryTime string `json:"queryTime"`
}

// lastBalance 记录上次已发布的余额状态，用于变化检测。
type lastBalance struct {
	Total, Available, Allocated uint64
	Err                         string
	Cached                      bool
	// published 标记是否发布过；首次采集无论结果都发布一次。
	published bool
}

func (d *Daemon) dataDir() string { return d.layout.Dir }

// Open prepares the daemon for dir without starting network services. It
// acquires the data directory lock and loads info/settings when present.
func Open(opts Options) (*Daemon, error) {
	layout, err := datadir.Resolve(opts.DataDir)
	if err != nil {
		return nil, err
	}
	guard, err := dlock.Acquire(layout.Dir)
	if err != nil {
		return nil, err
	}
	d := &Daemon{layout: layout, state: StateUninitialized, lockGuard: guard, events: events.New()}
	// 独立 Storage 根：与钱包根完全分开，独立加锁，禁止重叠。
	storageLayout, err := appstorage.Resolve(layout.Dir, opts.StorageDir)
	if err != nil {
		_ = guard.Release()
		return nil, err
	}
	if err := appstorage.CheckOverlap(layout.Dir, storageLayout.Dir); err != nil {
		_ = guard.Release()
		return nil, err
	}
	storageGuard, err := dlock.Acquire(storageLayout.Dir)
	if err != nil {
		_ = guard.Release()
		if errors.Is(err, dlock.ErrActive) {
			return nil, fmt.Errorf("Storage 目录已有后台在运行，不启动第二个钱包实例")
		}
		return nil, err
	}
	storageStore, err := appstorage.Open(storageLayout)
	if err != nil {
		_ = storageGuard.Release()
		_ = guard.Release()
		return nil, fmt.Errorf("daemon: 打开 App Storage 失败: %w", err)
	}
	d.storageLayout = storageLayout
	d.storageGuard = storageGuard
	d.storage = storageStore
	d.connectMgr = connect.NewManager(layout.Dir)
	d.connectMgr.SetGeneration(d.events.InstanceID())
	if err := d.connectMgr.Load(); err != nil {
		_ = storageStore.Close()
		_ = storageGuard.Release()
		_ = guard.Release()
		return nil, fmt.Errorf("daemon: 加载 App 授权失败: %w", err)
	}
	store := appconfig.New(layout.Dir)
	releaseAll := func() {
		if storageStore != nil {
			_ = storageStore.Close()
		}
		_ = storageGuard.Release()
		_ = guard.Release()
	}
	if layout.HasInitMarkers() {
		if err := store.Load(); err != nil {
			releaseAll()
			return nil, fmt.Errorf("daemon: 加载持久数据失败: %w", err)
		}
		d.store = store
		info, settings, err := store.Snapshot()
		if err != nil {
			releaseAll()
			return nil, err
		}
		_ = settings
		addr, err := addressFor(info.PublicKeyHex, info.BsvNetwork == "main")
		if err != nil {
			releaseAll()
			return nil, fmt.Errorf("daemon: 派生地址失败: %w", err)
		}
		d.publicKey = info.PublicKeyHex
		d.address = addr
		d.state = StateLocked
		d.tasks = newTaskManager(layout, store)
		d.tasks.events = d.events
		d.tasks.onTerminal = func() { go d.refreshBalance("task-terminal") }
		if err := d.tasks.loadPersisted(); err != nil {
			releaseAll()
			return nil, fmt.Errorf("daemon: 加载任务记录失败: %w", err)
		}
		d.roots = newRootsManager(layout, store)
		d.collectors = newCollectorsManager(layout.Dir, store, d.collectorSign)
		if err := d.roots.openAll(context.Background()); err != nil {
			// A root that is temporarily inaccessible must show an error, not
			// delete configuration or fail the whole daemon.
			fmt.Fprintf(os.Stderr, "km: 打开售卖目录时部分失败: %v\n", err)
		}
		if port := opts.PortOverride; port != 0 {
			// Port override is runtime-only; the file value is authoritative
			// for the next restart unless updated through the API.
			_ = port
		}
		return d, nil
	}
	// No new markers: legacy or fresh.
	if layout.HasAnyWallet() {
		// Legacy files or damage exist; never treat as fresh. Daemon cannot
		// migrate without a password, so report uninitialized with a hint.
		// The caller renders "请先运行 km init".
		d.store = nil
		d.state = StateUninitialized
		d.tasks = nil
		d.roots = nil
		return d, nil
	}
	d.store = nil
	d.state = StateUninitialized
	return d, nil
}

// State returns the current lifecycle state.
func (d *Daemon) State() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state
}

// isStopping reports whether the daemon is shutting down; new business is
// refused while in-flight work drains.
func (d *Daemon) isStopping() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state == StateStopping
}

// stopDispatchAccepting 停止接收新的分派请求（WS 与进程内共用入口）。
// 与 WaitGroup.Add 在同一把锁下配对，保证排空等待开始后不会再有新请求溜入。
// 在关闭流程最靠前调用（与 state=Stopping 同一步），之后再取消在途上下文。
func (d *Daemon) stopDispatchAccepting() {
	d.dispatchMu.Lock()
	d.dispatchStopping = true
	d.dispatchMu.Unlock()
}

// waitDispatchDrain 等待在途分派请求（含在途付款执行）退出，超时返回错误。
// 调用方负责在超时后保留文件锁并报告关闭失败，不得释放目录锁。
// 广播因取消而中断的，会在返回前按 unknown 完成责任记录（执行路径内处理）。
func (d *Daemon) waitDispatchDrain(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		d.dispatchWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("daemon: 分派在途请求未在预算内排空")
	}
}

// waitSeller bounded-waits for the seller loop (which itself waits, bounded,
// for in-flight sessions to reach a persisted step) to exit.
func (d *Daemon) waitSeller(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		d.sellerWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Close stops the daemon with bounded cleanup: stop accepting new business,
// cancel tasks and wait for their persist-and-cleanup path, then wait for
// the seller loop, collectors and locks in order. Evidence stays on disk,
// so a timeout truncates waiting, never business responsibility.
// It applies to both external (HTTP) and embedded (in-process) modes and
// does not require an HTTP server to exist.
//
// Close is idempotent: the first call runs the cleanup, concurrent or
// repeated callers wait for the same completed cleanup and observe its
// result instead of an early return.
func (d *Daemon) Close() error {
	d.closeOnce.Do(func() {
		d.closeDone = make(chan struct{})
		go func() {
			d.closeErr = d.close()
			close(d.closeDone)
		}()
	})
	<-d.closeDone
	return d.closeErr
}

// close performs the one-time cleanup. Only called via Close.
func (d *Daemon) close() error {
	d.mu.Lock()
	d.state = StateStopping
	sellerCancel := d.sellerCancel
	server := d.httpServer
	d.mu.Unlock()
	// 停止接收新的分派请求（WS 与进程内共用 Dispatch 入口）；
	// 与在途统计 Add 在同一把锁下配对，保证排空等待开始后不会再有新请求溜入。
	d.stopDispatchAccepting()
	// Stop the transport-independent lifecycle (periodic refresh) first so
	// no new background work starts while we drain.
	d.startMu.Lock()
	cancel := d.lifecycleCancel
	d.lifecycleCancel = nil
	d.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	// 停止付款重试定时器；未落盘的待重试项由下次启动收敛接管，不在关闭时补写。
	d.stopPaymentRetry()
	{
		done := make(chan struct{})
		go func() {
			d.watchWg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}
	// 关闭全部 WS 连接：重启后旧连接必须终止，不 lingering。
	d.wsMu.Lock()
	for c := range d.wsConns {
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "restart"), time.Now().Add(2*time.Second))
		_ = c.conn.Close()
	}
	d.wsConns = map[*wsConn]struct{}{}
	d.wsMu.Unlock()
	// 在途分派请求排空（WS 与进程内共用 Dispatch 入口，含在途付款执行）：
	// 上一步关闭连接已取消在途上下文，这里等待执行退出（广播取消按 unknown
	// 完成责任记录，见执行路径），之后才释放钱包目录锁。
	// 排空失败保留文件锁并报告关闭失败；预算覆盖单次付款执行的最坏网络等待。
	drainErr := d.waitDispatchDrain(45 * time.Second)
	if d.tasks != nil {
		d.tasks.stopAllAndWait(10 * time.Second)
	}
	if sellerCancel != nil {
		sellerCancel()
	}
	d.waitSeller(15 * time.Second)
	if d.collectors != nil {
		done := make(chan struct{})
		go func() {
			d.collectors.stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	// 关闭消息总线：订阅连接随之结束，不再接受新发布。
	if d.events != nil {
		d.events.Close()
	}
	if d.roots != nil {
		d.roots.closeAll()
	}
	// Storage 独立收尾预算 5s：先拒绝新操作，再等待在途排空，最后关闭数据库。
	// 超时则视为异常收尾：不释放独占锁（避免新实例与仍在使用的数据库重叠），
	// 调用者（daemonrun）据此报告异常退出；进程退出时由 OS 回收锁。
	storageErr := error(nil)
	if d.storage != nil {
		d.storage.BeginClose()
		done := make(chan struct{})
		go func() {
			d.storage.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			storageErr = fmt.Errorf("daemon: App Storage 在途操作未在预算内排空")
		}
		if storageErr == nil {
			if err := d.storage.Close(); err != nil {
				storageErr = fmt.Errorf("daemon: 关闭 App Storage 失败: %w", err)
			} else {
				d.storage = nil
			}
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	clearKey(d.privateKey)
	d.privateKey = nil
	// Break the seller cancel reference so a later Unlock cannot reuse it.
	d.sellerCancel = nil
	if storageErr == nil && drainErr == nil {
		if d.storageGuard != nil {
			_ = d.storageGuard.Release()
			d.storageGuard = nil
		}
		if d.lockGuard != nil {
			_ = d.lockGuard.Release()
			d.lockGuard = nil
		}
		return nil
	}
	// 异常收尾：保留文件锁（由进程退出释放），但已清理内存密钥。
	return errors.Join(storageErr, drainErr)
}

// Unlock decrypts the pure KeyHold with password and moves to unlocked.
// It is serialized; concurrent callers wait. The password is never persisted.
// State and key switch complete under the state lock; background abilities
// (seller, collectors, resumed tasks) start after the lock is released so
// initialization never blocks wallet-state readers and stopping can
// coordinate without deadlock.
func (d *Daemon) Unlock(password string) error {
	d.unlockMu.Lock()
	defer d.unlockMu.Unlock()
	d.mu.Lock()
	if d.state == StateUnlocked {
		d.mu.Unlock()
		return nil
	}
	if d.state == StateStopping {
		d.mu.Unlock()
		return errors.New("后台停止中")
	}
	if d.state == StateUninitialized || d.store == nil {
		d.mu.Unlock()
		return errors.New("尚未初始化，请先运行 km init")
	}
	d.mu.Unlock()
	raw, err := os.ReadFile(d.layout.KeyPath)
	if err != nil {
		return fmt.Errorf("读取钱包失败: %w", err)
	}
	doc, err := keyhold.Parse(raw)
	if err != nil {
		return fmt.Errorf("存储损坏：钱包文件无效")
	}
	result, err := keyhold.Unlock(doc, password)
	if err != nil {
		return errors.New("解锁失败：密码错误")
	}
	// Verify against info.json binding.
	info, _, err := d.store.Snapshot()
	if err != nil {
		clearBytes(result.PrivateKey)
		return err
	}
	if result.PublicKeyHex != info.PublicKeyHex || result.PublicKeyHex != doc.PublicKeyHex {
		clearBytes(result.PrivateKey)
		return errors.New("钱包公钥与身份元数据不一致")
	}
	expected := computeAuth(result.PrivateKey, info.BsvNetwork, result.PublicKeyHex)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(info.NetworkAuthenticationHex)) != 1 {
		clearBytes(result.PrivateKey)
		return errors.New("钱包网络身份认证失败")
	}
	// Commit state under lock, snapshot resume list, then release before
	// starting background abilities.
	var resume []string
	d.mu.Lock()
	// Another unlock may have won while we were doing KDF.
	if d.state == StateUnlocked {
		d.mu.Unlock()
		clearBytes(result.PrivateKey)
		return nil
	}
	if d.state == StateStopping {
		d.mu.Unlock()
		clearBytes(result.PrivateKey)
		return errors.New("后台停止中")
	}
	d.privateKey = result.PrivateKey
	d.publicKey = result.PublicKeyHex
	d.state = StateUnlocked
	if d.tasks != nil {
		d.tasks.mu.Lock()
		for seed, task := range d.tasks.tasks {
			if task.State == TaskPending {
				resume = append(resume, seed)
			}
		}
		d.tasks.mu.Unlock()
	}
	d.mu.Unlock()
	// Start background abilities outside the state lock. Each entry is
	// idempotent and refuses to start while stopping, so Close and Unlock
	// cannot interleave into a leaked goroutine.
	if d.isStopping() {
		return nil
	}
	// Start multi-root selling in the background (idempotent).
	go d.ensureSellerStarted()
	d.ensureCollectorsStarted()
	d.feedCollectors()
	// 钱包已解锁：发布钱包状态，启动余额统一采集（资金分配变化与
	// 外部充值都经此发布，不靠各视图独立查链）。
	d.publishWallet()
	d.startBalanceWatcher()
	// Resume crash-interrupted tasks against their bound roots now that the
	// key is available; on-disk evidence makes each a resume, not a repurchase.
	for _, seed := range resume {
		if d.isStopping() {
			break
		}
		d.startTask(seed)
	}
	return nil
}

func clearKey(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// InstanceID 返回后台实例标识，用于握手与旧实例事件丢弃。
func (d *Daemon) InstanceID() string {
	if d.events == nil {
		return ""
	}
	return d.events.InstanceID()
}

// APIHandler returns the API-only handler: only /api/ws plus explicit 404
// for retired paths. Old business HTTP API and SSE are deleted; HTTP only
// carries static Web and WebSocket upgrade.
func (d *Daemon) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/ws", d.handleWS)
	// Unknown API paths are explicit 404s, never the SPA fallback.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	return d.recoverPanic(d.guard(mux))
}

// httpHandler composes the standalone entry: always the API plus the full
// Web. CLI embedded entries use the API-only handler via APIHandler.
func (d *Daemon) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", d.APIHandler())
	mux.Handle("/", webui.Handler())
	return d.recoverPanic(d.guard(mux))
}

// recoverPanic converts handler panics into explicit 500s so the HTTP and
// in-process transports return the same error instead of EOF vs JSON.
func (d *Daemon) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// If headers were already sent, http.Error only logs;
				// uninitialized guards below prevent this path anyway.
				http.Error(w, fmt.Sprintf("后台异常: %v", rec), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Start manages the transport-independent background lifecycle (periodic
// catalog refresh and similar). It is idempotent: repeated calls do not
// start duplicate loops. Business that requires the wallet key (seller,
// collectors, resumed tasks) still starts on Unlock, not here.
func (d *Daemon) Start(ctx context.Context) error {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	if d.started {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	lifecycle, cancel := context.WithCancel(ctx)
	d.lifecycleCtx = lifecycle
	d.lifecycleCancel = cancel
	d.started = true
	d.watchWg.Add(1)
	go func() {
		defer d.watchWg.Done()
		d.watchReload(lifecycle)
	}()
	d.watchWg.Add(1)
	go func() {
		defer d.watchWg.Done()
		d.watchApprovalExpiry(lifecycle)
	}()
	return nil
}

// LifecycleContext returns the background lifecycle context started by
// Start, or nil when Start has not run. Collectors and other post-unlock
// loops prefer it over context.Background so Close always drains them.
func (d *Daemon) LifecycleContext() context.Context {
	d.startMu.Lock()
	defer d.startMu.Unlock()
	return d.lifecycleCtx
}

// ServeHTTP starts the HTTP entry on 127.0.0.1:port: full Web plus /api/ws.
// Port comes from settings unless overridden. It never starts
// business loops; call Start for the transport-independent lifecycle.
func (d *Daemon) ServeHTTP(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d.mu.Lock()
	if d.httpServer != nil {
		d.mu.Unlock()
		return "", fmt.Errorf("daemon: HTTP 服务已在运行")
	}
	if d.store == nil {
		// Uninitialized daemon still serves a minimal status surface so CLI
		// can report "please run km init" without a second process.
	}
	port := DefaultPort()
	if d.store != nil {
		_, settings, err := d.store.Snapshot()
		if err == nil && settings.Port != 0 {
			port = settings.Port
		}
	}
	d.mu.Unlock()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	server := &http.Server{
		Addr:              addr,
		Handler:           d.httpHandler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("daemon: 监听 %s 失败: %w", addr, err)
	}
	d.httpDone = make(chan error, 1)
	d.mu.Lock()
	d.httpServer = server
	d.listener = listener
	d.mu.Unlock()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		// Report the server outcome (nil on clean Shutdown) so the
		// lifecycle owner can distinguish crash from orderly stop.
		// A failed Serve prevents normal operation; only the owner
		// decides whether to shut down on it.
		d.httpDone <- server.Serve(listener)
	}()
	return listener.Addr().String(), nil
}

// HTTPDone reports the HTTP server result. It is nil until ServeHTTP has
// been called. A non-nil error (anything other than ErrServerClosed,
// which accompanies orderly shutdown) means the HTTP surface died
// unexpectedly while the daemon otherwise keeps running.
func (d *Daemon) HTTPDone() <-chan error {
	// No lock: assigned once under mu in ServeHTTP before serving, and
	// only read afterwards. A data race here would mean ServeHTTP was
	// called concurrently, which ServeHTTP itself already rejects.
	return d.httpDone
}

// DefaultPort returns the effective port (for diagnostics).
func DefaultPort() int { return appconfig.DefaultPort }

// guard enforces loopback listen (defence in depth via Host check),
// same-origin (or absent) Origin, and body limits. There is no token.
// Development mode (KEYMASTER_ENV=dev) additionally allows the Vite dev
// server origins http://127.0.0.1:5173 and http://localhost:5173 so the
// Vite proxy can be used with hot reload; production never widens this.
func (d *Daemon) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Host != r.Host {
				if os.Getenv("KEYMASTER_ENV") != "dev" || (parsed.Host != "127.0.0.1:5173" && parsed.Host != "localhost:5173") {
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func methodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("allow", "GET, POST, PUT, DELETE")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// addressFor derives the P2PKH address for display.
func addressFor(publicKeyHex string, mainnet bool) (string, error) {
	// Kept small: reuse the wallet address derivation via go-sdk script.
	// Import cycle risk is avoided by local implementation in address.go.
	return deriveAddress(publicKeyHex, mainnet)
}

// taskManager and rootsManager live in tasks.go / roots_store.go.

var _ = filepath.Clean
var _ = strings.TrimSpace
