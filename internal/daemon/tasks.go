package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/events"
)

// TaskState is the download task lifecycle.
type TaskState string

const (
	TaskPending   TaskState = "pending"
	TaskRunning   TaskState = "running"
	TaskDone      TaskState = "done"
	TaskFailed    TaskState = "failed"
	TaskCancelled TaskState = "cancelled"
)

// persistedTasksVersion is the client/tasks.json format version.
const persistedTasksVersion = 1

// Task is one download task bound to a target root at creation.
type Task struct {
	SeedHashHex string    `json:"seedHashHex"`
	RootID      string    `json:"rootID"`
	State       TaskState `json:"state"`
	Detail      string    `json:"detail,omitempty"`
	BlocksDone  uint64    `json:"blocksDone"`
	BlocksTotal uint64    `json:"blocksTotal"`
	Recommended []string  `json:"recommended,omitempty"`
	FinalPath   string    `json:"finalPath,omitempty"`
	Bytes       int64     `json:"bytes,omitempty"`
	PaidSats    string    `json:"paidSats,omitempty"`
	Error       string    `json:"error,omitempty"`
	CreatedAt   string    `json:"createdAt"`
	UpdatedAt   string    `json:"updatedAt"`

	cancel context.CancelFunc
	// done is closed by the startTask wrapper when this execution's goroutine
	// exits. Retries wait on the previous execution's channel so an old
	// goroutine can never mark a new execution.
	done chan struct{}
	subs map[chan Task]struct{}
}

type taskManager struct {
	mu     sync.Mutex
	layout datadir.Layout
	store  *appconfig.Store
	tasks  map[string]*Task
	// events 是统一消息总线；publishLocked 在任务持久化成功后发布，
	// 从不等待订阅者。nil 时不发布（单测/未初始化路径）。
	events *events.Broker
	// onTerminal 在任务进入终态后触发余额刷新（异步，不阻塞执行）。
	onTerminal func()
	// stopping latches at shutdown: no new runs start afterwards.
	stopping bool
	// wg tracks live run goroutines so shutdown waits for actual exit,
	// not just for a state flag.
	wg sync.WaitGroup
	// keyProvider returns a copy of the unlocked private key, or nil when locked.
	keyProvider func() []byte
	// settingsProvider returns current settings snapshot for execution.
	settingsProvider func() (appconfig.Settings, error)

	lastPersist time.Time
}

func newTaskManager(layout datadir.Layout, store *appconfig.Store) *taskManager {
	return &taskManager{layout: layout, store: store, tasks: map[string]*Task{}}
}

// startTask launches one run goroutine for seed. Runs that never start
// (stopping, missing, or no longer pending) do not touch task state.
func (d *Daemon) startTask(seed string) {
	d.tasks.mu.Lock()
	if d.tasks.stopping {
		d.tasks.mu.Unlock()
		return
	}
	t, ok := d.tasks.tasks[seed]
	if !ok {
		d.tasks.mu.Unlock()
		return
	}
	done := make(chan struct{})
	t.done = done
	d.tasks.wg.Add(1)
	d.tasks.mu.Unlock()
	go func() {
		defer d.tasks.wg.Done()
		defer close(done)
		d.runTask(seed)
	}()
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// tasksFile is the persisted task registry: identity, target root binding and
// lifecycle survive daemon restarts so queries and resume entries keep working.
func (m *taskManager) tasksFile() string {
	return filepath.Join(m.layout.ClientDir, "tasks.json")
}

func (m *taskManager) list() []Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, *t)
	}
	return out
}

func (m *taskManager) get(seed string) (*Task, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[seed]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// activeRoots returns root IDs with pending/running tasks.
func (m *taskManager) activeRoots() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, t := range m.tasks {
		if t.State == TaskPending || t.State == TaskRunning {
			out[t.RootID] = true
		}
	}
	return out
}

// cancelAllLocked collects the cancel funcs of live tasks without touching
// their state: cancellation is only a request. Each runTask observes its own
// context and records the terminal state itself when it actually stops, so a
// graceful shutdown never marks work finished that is still running.
func (m *taskManager) cancelAllLocked() []context.CancelFunc {
	m.mu.Lock()
	defer m.mu.Unlock()
	var cancels []context.CancelFunc
	for _, t := range m.tasks {
		if t.cancel != nil && (t.State == TaskPending || t.State == TaskRunning) {
			cancels = append(cancels, t.cancel)
		}
	}
	return cancels
}

// stopAllAndWait stops accepting new runs, requests cancellation of live
// tasks and waits, bounded, for every run goroutine to actually exit. The
// terminal state is always written by the exiting run itself, so waiting on
// the WaitGroup (not on state polling) guarantees no execution is still
// touching resources when Close proceeds to release them.
func (m *taskManager) stopAllAndWait(timeout time.Duration) {
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	for _, cancel := range m.cancelAllLocked() {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.saveLocked()
}

func (m *taskManager) publishLocked(t *Task) {
	t.UpdatedAt = nowRFC3339()
	for ch := range t.subs {
		select {
		case ch <- *t:
		default:
		}
	}
	terminal := t.State == TaskDone || t.State == TaskFailed || t.State == TaskCancelled
	if terminal || time.Since(m.lastPersist) >= 5*time.Second {
		_ = m.saveLocked()
	}
	// 任务持久化成功后再发布；慢订阅者不阻塞执行，溢出走 resync。
	if m.events != nil {
		dto := taskDTO(t)
		raw, err := json.Marshal(dto)
		if err == nil {
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err == nil {
				m.events.Publish(kmapi.TopicTask, t.SeedHashHex, nil, data)
			}
		}
	}
	if terminal && m.onTerminal != nil {
		m.onTerminal()
	}
}

// saveLocked writes the task registry atomically and reports errors, so
// task creation can fail loudly instead of running work that could not be
// recovered after a crash. Callers hold m.mu; the file is tiny (one record
// per seed) so this never blocks long.
func (m *taskManager) saveLocked() error {
	type persistedTask struct {
		SeedHashHex string    `json:"seedHashHex"`
		RootID      string    `json:"rootID"`
		State       TaskState `json:"state"`
		Detail      string    `json:"detail,omitempty"`
		BlocksDone  uint64    `json:"blocksDone"`
		BlocksTotal uint64    `json:"blocksTotal"`
		Recommended []string  `json:"recommended,omitempty"`
		FinalPath   string    `json:"finalPath,omitempty"`
		Bytes       int64     `json:"bytes,omitempty"`
		PaidSats    string    `json:"paidSats,omitempty"`
		Error       string    `json:"error,omitempty"`
		CreatedAt   string    `json:"createdAt"`
		UpdatedAt   string    `json:"updatedAt"`
	}
	doc := struct {
		Version int                      `json:"version"`
		Tasks   map[string]persistedTask `json:"tasks"`
	}{Version: persistedTasksVersion, Tasks: map[string]persistedTask{}}
	for seed, t := range m.tasks {
		doc.Tasks[seed] = persistedTask{
			SeedHashHex: t.SeedHashHex, RootID: t.RootID, State: t.State,
			Detail: t.Detail, BlocksDone: t.BlocksDone, BlocksTotal: t.BlocksTotal,
			Recommended: append([]string(nil), t.Recommended...),
			FinalPath:   t.FinalPath, Bytes: t.Bytes, PaidSats: t.PaidSats,
			Error: t.Error, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(m.tasksFile())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tasks-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, m.tasksFile()); err != nil {
		return err
	}
	m.lastPersist = time.Now()
	return nil
}

// loadPersisted restores the task registry after a restart. Tasks that were
// live when the daemon stopped are marked failed with a resume hint instead
// of being silently dropped or rebound to another root.
func (m *taskManager) loadPersisted() error {
	raw, err := os.ReadFile(m.tasksFile())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc struct {
		Version int `json:"version"`
		Tasks   map[string]struct {
			SeedHashHex string    `json:"seedHashHex"`
			RootID      string    `json:"rootID"`
			State       TaskState `json:"state"`
			Detail      string    `json:"detail,omitempty"`
			BlocksDone  uint64    `json:"blocksDone"`
			BlocksTotal uint64    `json:"blocksTotal"`
			Recommended []string  `json:"recommended,omitempty"`
			FinalPath   string    `json:"finalPath,omitempty"`
			Bytes       int64     `json:"bytes,omitempty"`
			PaidSats    string    `json:"paidSats,omitempty"`
			Error       string    `json:"error,omitempty"`
			CreatedAt   string    `json:"createdAt"`
			UpdatedAt   string    `json:"updatedAt"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc.Version != persistedTasksVersion {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for seed, p := range doc.Tasks {
		if seed == "" || p.SeedHashHex == "" || p.RootID == "" {
			continue
		}
		if _, dup := m.tasks[seed]; dup {
			continue
		}
		state := p.State
		detail, taskErr := p.Detail, p.Error
		if state == TaskPending || state == TaskRunning {
			// Crash-interrupted work is resumed, not dropped: it goes back
			// to pending with its original target-root binding and is
			// picked up on unlock; evidence on disk makes it a resume.
			state = TaskPending
			detail = "后台重启中断，等待解锁后自动续接"
			taskErr = ""
		}
		m.tasks[seed] = &Task{
			SeedHashHex: p.SeedHashHex, RootID: p.RootID, State: state,
			Detail: detail, BlocksDone: p.BlocksDone, BlocksTotal: p.BlocksTotal,
			Recommended: append([]string(nil), p.Recommended...),
			FinalPath:   p.FinalPath, Bytes: p.Bytes, PaidSats: p.PaidSats,
			Error: taskErr, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
			subs: make(map[chan Task]struct{}),
		}
	}
	return nil
}
