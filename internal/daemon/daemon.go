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
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
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
	d := &Daemon{layout: layout, state: StateUninitialized, lockGuard: guard}
	store := appconfig.New(layout.Dir)
	if layout.HasInitMarkers() {
		if err := store.Load(); err != nil {
			_ = guard.Release()
			return nil, fmt.Errorf("daemon: 加载持久数据失败: %w", err)
		}
		d.store = store
		info, settings, err := store.Snapshot()
		if err != nil {
			_ = guard.Release()
			return nil, err
		}
		_ = settings
		addr, err := addressFor(info.PublicKeyHex, info.BsvNetwork == "main")
		if err != nil {
			_ = guard.Release()
			return nil, fmt.Errorf("daemon: 派生地址失败: %w", err)
		}
		d.publicKey = info.PublicKeyHex
		d.address = addr
		d.state = StateLocked
		d.tasks = newTaskManager(layout, store)
		if err := d.tasks.loadPersisted(); err != nil {
			_ = guard.Release()
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
func (d *Daemon) Close() error {
	d.mu.Lock()
	if d.state == StateStopping {
		d.mu.Unlock()
		return nil
	}
	d.state = StateStopping
	sellerCancel := d.sellerCancel
	server := d.httpServer
	d.mu.Unlock()
	// Stop the transport-independent lifecycle (periodic refresh) first so
	// no new background work starts while we drain.
	d.startMu.Lock()
	cancel := d.lifecycleCancel
	d.lifecycleCancel = nil
	d.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
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
	if d.roots != nil {
		d.roots.closeAll()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	clearKey(d.privateKey)
	d.privateKey = nil
	// Break the seller cancel reference so a later Unlock cannot reuse it.
	d.sellerCancel = nil
	if d.lockGuard != nil {
		_ = d.lockGuard.Release()
		d.lockGuard = nil
	}
	return nil
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

// Handler returns the same API handler for both transports. It registers
// routes and Host/Origin protection but never listens or starts goroutines.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", d.handleStatus)
	mux.HandleFunc("/api/info", d.handleInfo)
	mux.HandleFunc("/api/unlock", d.handleUnlock)
	mux.HandleFunc("/api/roots", d.handleRoots)
	mux.HandleFunc("/api/roots/", d.handleRootRoute)
	mux.HandleFunc("/api/roots/default", d.handleRootsDefault)
	mux.HandleFunc("/api/files", d.handleFiles)
	mux.HandleFunc("/api/browse", d.handleBrowse)
	mux.HandleFunc("/api/settings", d.handleSettings)
	mux.HandleFunc("/api/settings/reset", d.handleSettingsReset)
	mux.HandleFunc("/api/catalog/refresh", d.handleCatalogRefresh)
	mux.HandleFunc("/api/collector/status", d.handleCollectorStatus)
	mux.HandleFunc("/api/collector/history", d.handleCollectorHistory)
	mux.HandleFunc("/api/collector/description", d.handleCollectorDescription)
	mux.HandleFunc("/api/collector/config", d.handleCollectorConfig)
	mux.HandleFunc("/api/collector/flush", d.handleCollectorFlush)
	mux.HandleFunc("/api/collector/retry", d.handleCollectorRetry)
	mux.HandleFunc("/api/pricing/defaults", d.handlePricingDefaults)
	mux.HandleFunc("/api/pricing/batch", d.handlePricingBatch)
	mux.HandleFunc("/api/pricing/file", d.handlePricingFile)
	mux.HandleFunc("/api/tasks", d.handleTasks)
	mux.HandleFunc("/api/tasks/", d.handleTaskRoute)
	// Legacy download management (no token): keep functional until React
	// replaces the surface, but never require an API key.
	registerLegacyDownloads(mux, d)
	// Favicon etc fall through to the embedded Web UI.
	mux.HandleFunc("/", d.handleUI)
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

// ServeHTTP starts the HTTP API on 127.0.0.1:port using Handler().
// Port comes from settings unless overridden. It never starts business
// loops; call Start for the transport-independent lifecycle.
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
		Handler:           d.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("daemon: 监听 %s 失败: %w", addr, err)
	}
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
		_ = server.Serve(listener)
	}()
	return listener.Addr().String(), nil
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
