package tray

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"

	"fyne.io/systray"
)

//go:embed icon.png
var trayIcon []byte

// RunChild is the child process entry point ("km internal-tray"). Stdin and
// stdout are the control pipe; stderr carries diagnostics. It returns the
// process exit code: 0 only after an orderly shutdown handshake.
func RunChild(stdin io.Reader, stdout io.Writer, logf func(string, ...any)) int {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if ok, reason := hasTrayHost(); !ok {
		fmt.Fprintf(os.Stderr, "km: 托盘不可用（%s），继续以后台模式运行\n", reason)
		return 2
	}
	out := &lockedWriter{w: stdout}
	send := func(m Message) {
		raw, err := Encode(m)
		if err != nil {
			return
		}
		_, _ = out.Write(raw)
	}
	var mu sync.Mutex
	var cur Status
	var statusItem, openItem *systray.MenuItem
	built := make(chan struct{})
	apply := func() {
		mu.Lock()
		st := cur
		si, oi := statusItem, openItem
		mu.Unlock()
		if si == nil || oi == nil {
			return // menu not built yet; onReady applies stored state
		}
		title := "状态：" + stateText(st)
		if st.Network != "" {
			title += "（" + st.Network + "）"
		}
		si.SetTitle(title)
		si.SetTooltip(title)
		if st.WebAdmin && isLoopbackHTTP(st.APIAddr) {
			oi.Enable()
		} else {
			oi.Disable()
		}
	}
	setStatus := func(s Status) {
		mu.Lock()
		cur = s
		mu.Unlock()
		select {
		case <-built:
			apply()
		default:
		}
	}
	// stdin loop: shutdown message or EOF ends the tray loop.
	closing := make(chan struct{})
	var closeOnce sync.Once
	quit := func() { closeOnce.Do(func() { close(closing) }) }
	go func() {
		fr := NewReader(bufio.NewReader(stdin))
		for {
			m, err := fr.Next()
			if err != nil {
				quit() // parent gone or fatal framing: exit, no orphan
				return
			}
			switch m.Type {
			case MsgStatus:
				setStatus(Status{
					State:    m.State,
					Locked:   m.Locked,
					Network:  m.Network,
					APIAddr:  m.APIAddr,
					WebAdmin: m.WebAdmin,
				})
			case MsgShutdown:
				quit()
				return
			default:
				// Unknown types are ignored, never fatal.
			}
		}
	}()
	systray.Run(func() {
		mu.Lock()
		statusItem = systray.AddMenuItem("状态：启动中", "后台状态")
		statusItem.Disable()
		systray.AddSeparator()
		openItem = systray.AddMenuItem("打开管理页面", "在浏览器中打开")
		openItem.Disable()
		restartItem := systray.AddMenuItem("重启后台", "有序重启后台")
		quitItem := systray.AddMenuItem("退出", "有序停止后台并退出")
		mu.Unlock()
		close(built)
		apply()
		send(Message{Type: MsgReady})
		go func() {
			for range openItem.ClickedCh {
				mu.Lock()
				addr, web := cur.APIAddr, cur.WebAdmin
				mu.Unlock()
				if !web || !isLoopbackHTTP(addr) {
					continue
				}
				if !openWeb(addr) {
					logf("托盘：无法打开管理页面")
				}
			}
		}()
		go func() {
			for range restartItem.ClickedCh {
				send(Message{Type: MsgAction, Action: ActionRestart})
			}
		}()
		go func() {
			for range quitItem.ClickedCh {
				send(Message{Type: MsgAction, Action: ActionQuit})
			}
		}()
		go func() {
			<-closing
			systray.Quit()
		}()
	}, func() {
		// onExit runs on shutdown; nothing to persist.
	})
	return 0
}

func stateText(s Status) string {
	switch s.State {
	case "unlocked":
		return "已解锁"
	case "restarting":
		return "重启中"
	case "stopping":
		return "停止中"
	default:
		return "已锁定"
	}
}

// isLoopbackHTTP reports whether rawURL is an http(s) loopback URL.
// Only parent-generated addresses are ever opened.
func isLoopbackHTTP(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// openWeb opens url in the system default browser.
func openWeb(rawURL string) bool {
	if !isLoopbackHTTP(rawURL) {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	return cmd.Process.Release() == nil
}

// lockedWriter serializes concurrent frame writes.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
