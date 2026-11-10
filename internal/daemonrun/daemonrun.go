// Package daemonrun unifies the background lifecycle: one place owns the
// order open → start → serve → ready → supervise → orderly close, for the
// foreground daemon, Windows services and the tray host alike.
//
// Rules enforced here:
//
//   - The HTTP listener must be up before the tray may show "running".
//   - Tray startup never blocks API readiness; tray failure is logged and
//     the backend keeps running without it.
//   - Signals, tray actions, service stops and HTTP failures converge into
//     one wait loop; there is no second cleanup path.
//   - Close is awaited to completion (not just initiated) before reporting
//     success, restarting, or unlocking the data directory for a new run.
//   - Restarts are serialized; a second request while restarting is dropped
//     with a log line instead of starting a second instance.
//   - The tray child is kept across backend restarts; it is respawned only
//     when no live child exists at backend start. A dead tray is never
//     restarted mid-run and never triggers backend shutdown.
package daemonrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/daemon"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
	"github.com/bsv8/keymaster.cc-cli/internal/tray"
)

const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitCancelled = 4
)

// TrayMode selects whether this run owns a tray child process.
type TrayMode int

const (
	// TrayAuto starts the tray child unless the platform or flags forbid
	// it. A tray that cannot start (no host, no display) is logged and the
	// backend keeps running headless; it is never a startup failure.
	TrayAuto TrayMode = iota
	// TrayNever never starts the tray child (services, --no-tray).
	TrayNever
)

// Options configures one daemonrun execution.
type Options struct {
	DataDir  string
	WebAdmin bool
	Tray     TrayMode
	// Logf receives lifecycle diagnostics (stderr in practice).
	Logf func(format string, args ...any)
	// ExitCodeOnSignal is returned after an orderly shutdown triggered by
	// context cancellation or the tray quit action. The foreground CLI
	// passes exitCancelled to keep its historic Ctrl-C code; services
	// leave the zero value (exitOK) so a requested stop reads as success.
	ExitCodeOnSignal int
}

// Run executes the full lifecycle and returns the process exit code.
// It only returns after cleanup is complete (or bounded-waited).
func Run(ctx context.Context, opts Options) int {
	if opts.Logf == nil {
		opts.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	c := &Controller{
		opts:      opts,
		restartCh: make(chan string, 1),
		actionsCh: make(chan string, 16),
	}
	return c.run(ctx)
}

// Controller owns one backend instance at a time plus an optional tray
// child that persists across backend restarts. Construct via Run.
type Controller struct {
	opts Options

	mu        sync.Mutex
	restartCh chan string
	restart   bool

	actionsCh chan string

	trayMu sync.Mutex
	tray   *tray.Child
}

// requestRestart queues an orderly restart. Concurrent requests collapse
// into the running one with a log line instead of a second instance.
func (c *Controller) requestRestart(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.restart {
		c.opts.Logf("km: 已在重启中，忽略本次重启请求（%s）", reason)
		return
	}
	select {
	case c.restartCh <- reason:
		c.restart = true
	default:
		c.opts.Logf("km: 已在重启中，忽略本次重启请求（%s）", reason)
	}
}

func (c *Controller) run(ctx context.Context) int {
	sighup := sighupChannel()
	for {
		code, restart := c.runOne(ctx, sighup)
		if !restart {
			return code
		}
		c.mu.Lock()
		c.restart = false
		c.mu.Unlock()
	}
}

// runOne starts one backend instance, supervises it, and shuts it down.
// It reports whether the caller should start a fresh instance afterwards.
func (c *Controller) runOne(ctx context.Context, sighup <-chan os.Signal) (int, bool) {
	layout, err := datadir.Resolve(c.opts.DataDir)
	if err != nil {
		c.opts.Logf("km: %v", err)
		return exitError, false
	}
	if !layout.HasInitMarkers() {
		if layout.HasAnyWallet() {
			c.opts.Logf("km: 数据不完整或为旧版，请先运行 km init（迁移或修复）")
		} else {
			c.opts.Logf("km: 尚未初始化，请先运行 km init")
		}
		return exitError, false
	}
	d, err := daemon.Open(daemon.Options{DataDir: layout.Dir, WebAdmin: c.opts.WebAdmin})
	if err != nil {
		if errors.Is(err, dlock.ErrActive) {
			c.opts.Logf("km: 数据目录已有后台在运行")
			return exitError, false
		}
		c.opts.Logf("km: %v", err)
		return exitError, false
	}
	if err := d.Start(ctx); err != nil {
		c.opts.Logf("km: %v", err)
		_ = d.Close()
		return exitError, false
	}
	addr, err := d.ServeHTTP(ctx)
	if err != nil {
		c.opts.Logf("km: %v", err)
		_ = d.Close()
		return exitError, false
	}
	webState := map[bool]string{true: "开", false: "关"}[c.opts.WebAdmin]
	c.opts.Logf("km 后台已启动（锁定状态，Web Admin: %s）: http://%s/ 数据目录: %s", webState, addr, layout.Dir)

	c.maybeStartTray()
	watcher := c.startStatusWatcher(d, addr)
	c.pushTrayStatus(d, "locked", addr)

	code, restart := c.supervise(ctx, d, sighup, addr)
	if restart {
		c.opts.Logf("km: 开始重启…")
	}
	watcher.stop()
	if restart {
		c.pushTrayStatus(d, "restarting", addr)
	} else {
		c.pushTrayStatus(d, "stopping", addr)
	}
	// Full cleanup and lock release complete before reporting done.
	if err := d.Close(); err != nil {
		c.opts.Logf("km: 收尾异常: %v", err)
		if !restart {
			code = exitError
		}
	}
	c.closeTray()
	return code, restart
}

// supervise waits for shutdown triggers. It returns the exit code and
// whether the caller should start a fresh instance.
func (c *Controller) supervise(ctx context.Context, d *daemon.Daemon, sighup <-chan os.Signal, addr string) (int, bool) {
	_ = addr
	httpDone := d.HTTPDone()
	for {
		select {
		case <-ctx.Done():
			c.opts.Logf("退出中/收尾中…")
			return c.opts.ExitCodeOnSignal, false
		case action := <-c.actionsCh:
			switch action {
			case string(tray.ActionQuit):
				c.opts.Logf("退出中/收尾中…")
				return c.opts.ExitCodeOnSignal, false
			case string(tray.ActionRestart):
				c.requestRestart("托盘")
			case string(tray.ActionOpenAdmin):
				// Opened by the child itself; nothing to do here.
			default:
				c.opts.Logf("km: 忽略未知托盘动作 %q", action)
			}
		case <-sighup:
			c.requestRestart("SIGHUP")
		case err := <-httpDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				c.opts.Logf("km: HTTP 服务意外停止（%v），有序退出…", err)
				return exitError, false
			}
			// Clean shutdown path already closing; keep waiting for the
			// explicit trigger below instead of exiting early.
			if c.isRestarting() {
				continue
			}
			c.opts.Logf("退出中/收尾中…")
			return c.opts.ExitCodeOnSignal, false
		case reason := <-c.restartCh:
			c.opts.Logf("km: 收到重启请求（%s）", reason)
			return exitOK, true
		}
	}
}

func (c *Controller) isRestarting() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restart
}

// maybeStartTray spawns the tray child when enabled and no live child
// exists. A live child is kept (backend restarts reuse it). Failure only
// logs; the backend keeps running headless.
func (c *Controller) maybeStartTray() {
	if c.opts.Tray == TrayNever {
		return
	}
	c.trayMu.Lock()
	defer c.trayMu.Unlock()
	if c.tray != nil {
		if alive, _ := c.tray.Exited(); !alive {
			return
		}
		c.tray = nil
	}
	exe, err := os.Executable()
	if err != nil {
		c.opts.Logf("km: 托盘启动失败（无法定位程序）: %v；以后台模式继续", err)
		return
	}
	child, err := tray.Start(exe, c.onTrayAction, c.opts.Logf)
	if err != nil {
		c.opts.Logf("km: 托盘启动失败: %v；以后台模式继续", err)
		return
	}
	// The API is already serving; waiting here only delays supervision,
	// never readiness. Timeout means headless continuation, not failure.
	if err := child.WaitReady(5 * time.Second); err != nil {
		c.opts.Logf("km: %v；以后台模式继续", err)
		_ = child.Close(3 * time.Second)
		return
	}
	c.tray = child
	c.opts.Logf("km: 托盘已就绪")
}

// onTrayAction forwards child menu actions without blocking the child's
// reader goroutine. Overflow drops with a log line, never a stall.
func (c *Controller) onTrayAction(a tray.Action) {
	select {
	case c.actionsCh <- string(a):
	default:
		c.opts.Logf("km: 托盘动作队列已满，丢弃 %q", string(a))
	}
}

// closeTray performs the orderly tray handshake after backend cleanup.
// A missing or already-dead child is normal (headless or crashed tray);
// a timed-out shutdown is reported as abnormal, never as success.
func (c *Controller) closeTray() {
	c.trayMu.Lock()
	child := c.tray
	c.tray = nil
	c.trayMu.Unlock()
	if child == nil {
		return
	}
	if err := child.Close(5 * time.Second); err != nil {
		c.opts.Logf("km: %v", err)
		return
	}
	c.opts.Logf("km: 托盘已退出")
}

// liveTray returns the current child, or nil when none is alive. A child
// that died on its own is logged once here, detached, and never restarted
// mid-run: the backend keeps running.
func (c *Controller) liveTray() *tray.Child {
	c.trayMu.Lock()
	defer c.trayMu.Unlock()
	if c.tray == nil {
		return nil
	}
	if exited, err := c.tray.Exited(); exited {
		c.opts.Logf("km: 托盘异常退出（%v），后台继续运行，不重启托盘", err)
		c.tray = nil
		return nil
	}
	return c.tray
}

// pushTrayStatus sends a full status snapshot to a live tray child.
// It never affects the backend: no child, dead child or send failure
// only logs.
func (c *Controller) pushTrayStatus(d *daemon.Daemon, state, addr string) {
	child := c.liveTray()
	if child == nil {
		return
	}
	st := tray.Status{WebAdmin: c.opts.WebAdmin}
	switch state {
	case "unlocked":
		st.State, st.Locked = "unlocked", false
	case "restarting":
		st.State, st.Locked = "restarting", true
	case "stopping":
		st.State, st.Locked = "stopping", true
	default:
		st.State, st.Locked = "locked", true
	}
	if s := queryInprocStatus(d); s != nil {
		if s.Network != nil {
			st.Network = *s.Network
		}
		if s.Locked != nil {
			st.Locked = *s.Locked
			if state == "locked" && !*s.Locked {
				st.State = "unlocked"
			}
		}
	}
	if addr != "" {
		st.APIAddr = "http://" + addr
	}
	if err := child.Send(tray.StatusMessage(st)); err != nil {
		c.opts.Logf("km: 托盘状态推送失败: %v", err)
	}
}

// queryInprocStatus reads /api/status through the daemon's own handler
// without sockets. A nil return only means "network unknown to the tray".
func queryInprocStatus(d *daemon.Daemon) *kmapi.Status {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/status", nil)
	rec := httptest.NewRecorder()
	d.APIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil
	}
	var s kmapi.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		return nil
	}
	return &s
}

// statusWatcher polls the in-proc status and pushes changes to the tray.
// The tray keeps no diff state; every push is a full snapshot.
type statusWatcher struct {
	done chan struct{}
	wg   sync.WaitGroup
}

func (c *Controller) startStatusWatcher(d *daemon.Daemon, addr string) *statusWatcher {
	w := &statusWatcher{done: make(chan struct{})}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		last := ""
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
			}
			s := queryInprocStatus(d)
			key := "unknown"
			if s != nil {
				locked := s.Locked != nil && *s.Locked
				net := ""
				if s.Network != nil {
					net = *s.Network
				}
				key = s.State + "|" + net + "|" + map[bool]string{true: "l", false: "u"}[locked]
			}
			if key != last {
				last = key
				state := "locked"
				if s != nil && (s.Locked == nil || !*s.Locked) && s.State == string(daemon.StateUnlocked) {
					state = "unlocked"
				}
				c.pushTrayStatus(d, state, addr)
			}
			// Reap a mid-run tray crash promptly instead of waiting for
			// the next push: logged once, backend unaffected.
			c.liveTray()
		}
	}()
	return w
}

func (w *statusWatcher) stop() {
	close(w.done)
	w.wg.Wait()
}
