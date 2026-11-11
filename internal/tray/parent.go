package tray

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Child manages one tray child process (the same km binary, hidden
// internal-tray entry). The child never touches wallet, config, locks or
// business APIs; it only renders status and forwards menu actions.
type Child struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	closed chan struct{}
	once   sync.Once
	exited error // process result once finished (nil only on clean exit 0)

	readyCh chan struct{}
	ready   bool

	logf func(format string, args ...any)
}

// Start spawns the tray child and begins reading its messages. Actions are
// delivered to onAction (called on the reader goroutine; must not block).
// Stdout of the child carries only protocol frames; its stderr is wired to
// the parent's stderr for diagnostics.
func Start(exePath string, onAction func(Action), logf func(string, ...any)) (*Child, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if exePath == "" {
		return nil, fmt.Errorf("tray: empty executable path")
	}
	cmd := exec.Command(exePath, "internal-tray")
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("tray: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("tray: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("tray: start child: %w", err)
	}
	c := &Child{
		cmd:     cmd,
		stdin:   stdin,
		closed:  make(chan struct{}),
		readyCh: make(chan struct{}),
		logf:    logf,
	}
	go c.readLoop(stdout, onAction)
	go c.waitLoop()
	return c, nil
}

func (c *Child) readLoop(r io.Reader, onAction func(Action)) {
	fr := NewReader(bufio.NewReader(r))
	readySent := false
	for {
		m, err := fr.Next()
		if err != nil {
			if !isEOF(err) {
				c.logf("tray: 忽略损坏帧: %v", err)
				continue
			}
			c.finish(fmt.Errorf("tray: 子进程管道关闭"))
			return
		}
		switch m.Type {
		case MsgReady:
			if !readySent {
				readySent = true
				close(c.readyCh)
			}
		case MsgAction:
			if onAction != nil && m.Action != "" {
				onAction(Action(m.Action))
			}
		case MsgError:
			c.logf("tray: 子进程报告: %s: %s", m.Code, m.Message)
		default:
			c.logf("tray: 忽略未知消息类型: %q", m.Type)
		}
	}
}

func isEOF(err error) bool {
	if err == io.EOF {
		return true
	}
	if err == io.ErrUnexpectedEOF {
		return true
	}
	return false
}

func (c *Child) waitLoop() {
	err := c.cmd.Wait()
	if err != nil {
		c.finish(fmt.Errorf("tray: 子进程退出: %w", err))
		return
	}
	if code := c.cmd.ProcessState.ExitCode(); code != 0 {
		c.finish(fmt.Errorf("tray: 子进程退出码 %d", code))
		return
	}
	c.finish(nil)
}

func (c *Child) finish(err error) {
	c.once.Do(func() {
		c.exited = err
		close(c.closed)
	})
}

// WaitReady blocks until the child reports ready, exits, or timeout.
// A timeout means the child never became operational (no icon host,
// missing display, crash on startup); the caller must continue headless.
func (c *Child) WaitReady(timeout time.Duration) error {
	select {
	case <-c.readyCh:
		return nil
	case <-c.closed:
		if c.exited != nil {
			return fmt.Errorf("tray: 子进程提前退出: %v", c.exited)
		}
		return fmt.Errorf("tray: 子进程已退出")
	case <-time.After(timeout):
		return fmt.Errorf("tray: 等待就绪超时")
	}
}

// Exited reports whether the child process has finished.
func (c *Child) Exited() (bool, error) {
	select {
	case <-c.closed:
		return true, c.exited
	default:
		return false, nil
	}
}

// Send writes one message to the child. It fails fast when the child is
// gone; callers must not treat a send error as backend failure.
func (c *Child) Send(m Message) error {
	raw, err := Encode(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return fmt.Errorf("tray: 子进程已退出")
	default:
	}
	if _, err := c.stdin.Write(raw); err != nil {
		return fmt.Errorf("tray: 发送失败: %w", err)
	}
	return nil
}

// Close performs the orderly shutdown handshake: ask the child to quit,
// wait for its exit bounded by timeout, and kill on overrun. A timed-out
// shutdown is reported as abnormal, never as success.
func (c *Child) Close(timeout time.Duration) error {
	_ = c.Send(Message{Type: MsgShutdown})
	select {
	case <-c.closed:
	case <-time.After(timeout):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			<-c.closed
		}
		return fmt.Errorf("tray: 关闭超时，已强制结束")
	}
	if c.exited != nil {
		return fmt.Errorf("tray: 异常结束: %v", c.exited)
	}
	c.mu.Lock()
	_ = c.stdin.Close()
	c.mu.Unlock()
	return nil
}

// Action is a menu action requested by the tray child.
type Action string

// Status describes what the tray shows. It is a full snapshot; the child
// keeps no diff state.
type Status struct {
	State    string // locked | unlocked | restarting | stopping
	Locked   bool
	Network  string
	APIAddr  string // "127.0.0.1:PORT" or "" when none
	Detail   string
}

// StatusMessage builds the wire message for s.
func StatusMessage(s Status) Message {
	state := s.State
	if state == "" {
		state = "locked"
	}
	return Message{
		Type:     MsgStatus,
		State:    state,
		Locked:   s.Locked,
		Network:  s.Network,
		APIAddr:  s.APIAddr,
				Detail:   s.Detail,
	}
}
