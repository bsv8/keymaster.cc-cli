package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleTasks creates or reuses a download task. Duplicate submissions for the
// same seed never start a second purchase: the existing task is returned.
func (d *Daemon) handleTasks(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]any{"tasks": d.tasks.list()})
	case http.MethodPost:
		d.mu.Lock()
		locked := d.state != StateUnlocked
		d.mu.Unlock()
		if locked {
			http.Error(w, "后台已锁定，请先解锁", http.StatusLocked)
			return
		}
		if d.isStopping() {
			http.Error(w, "后台停止中，不再接收新任务", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			SeedHashHex string `json:"seedHashHex"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		seed := strings.ToLower(strings.TrimSpace(body.SeedHashHex))
		if !validSeedHash(seed) {
			http.Error(w, "seedHashHex 无效", http.StatusBadRequest)
			return
		}
		task, created, err := d.ensureTask(r.Context(), seed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if created {
			w.WriteHeader(http.StatusCreated)
		}
		writeJSON(w, task)
	default:
		methodNotAllowed(w)
	}
}

func (d *Daemon) handleTaskRoute(w http.ResponseWriter, r *http.Request) {
	if d.store == nil || d.tasks == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	// /api/tasks/<seed> , /api/tasks/<seed>/events , /api/tasks/<seed>/cancel
	rest := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	seed := strings.ToLower(parts[0])
	if len(parts) == 2 && parts[1] == "events" {
		d.serveTaskEvents(w, r, seed)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if err := d.cancelTask(seed); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	t, ok := d.tasks.get(seed)
	if !ok {
		// Fall back to persisted evidence so a restarted daemon can still
		// report completed tasks.
		if persisted := d.loadPersistedTask(seed); persisted != nil {
			writeJSON(w, persisted)
			return
		}
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, t)
}

func (d *Daemon) ensureTask(ctx context.Context, seed string) (*Task, bool, error) {
	info, _, err := d.store.Snapshot()
	if err != nil {
		return nil, false, err
	}
	if info.DefaultDownloadRootID == "" {
		return nil, false, fmt.Errorf("没有默认下载根，请先配置")
	}
	// Validate the default root is still present and accessible.
	found := false
	for _, root := range info.Roots {
		if root.ID == info.DefaultDownloadRootID {
			found = true
			break
		}
	}
	if !found {
		return nil, false, fmt.Errorf("默认下载根不存在，请重新配置")
	}
	if h, ok := d.roots.get(info.DefaultDownloadRootID); !ok || h.catalog == nil {
		return nil, false, fmt.Errorf("默认下载根不可访问")
	}
	d.tasks.mu.Lock()
	if existing, ok := d.tasks.tasks[seed]; ok {
		switch existing.State {
		case TaskDone, TaskPending, TaskRunning:
			cp := *existing
			d.tasks.mu.Unlock()
			return &cp, false, nil
		case TaskFailed, TaskCancelled:
			// Explicit resubmission retries: wait for the old execution to
			// actually exit first (its done channel), then reset to pending
			// and drive a new run against the originally bound target root.
			// This keeps a late old callback from marking the new run, and
			// the new run never collides with the old seed lock.
			oldDone := existing.done
			oldCancel := existing.cancel
			d.tasks.mu.Unlock()
			if oldCancel != nil {
				oldCancel()
			}
			if oldDone != nil {
				select {
				case <-oldDone:
				case <-time.After(15 * time.Second):
					return nil, false, fmt.Errorf("旧执行仍在收尾，请稍后重试")
				}
			}
			d.tasks.mu.Lock()
			existing, ok = d.tasks.tasks[seed]
			if !ok {
				d.tasks.mu.Unlock()
				return nil, false, fmt.Errorf("任务不存在")
			}
			if existing.State != TaskFailed && existing.State != TaskCancelled {
				cp := *existing
				d.tasks.mu.Unlock()
				return &cp, false, nil
			}
			prev := *existing
			existing.State = TaskPending
			existing.Error = ""
			existing.Detail = "任务重试"
			existing.BlocksDone = 0
			existing.BlocksTotal = 0
			existing.FinalPath = ""
			existing.Bytes = 0
			existing.PaidSats = ""
			existing.Recommended = nil
			// Retry persists like creation: the new run must not start
			// before its binding is safely on disk. On failure roll the
			// terminal state back instead of losing it.
			if err := d.tasks.saveLocked(); err != nil {
				*existing = prev
				d.tasks.mu.Unlock()
				return nil, false, fmt.Errorf("任务落盘失败: %w", err)
			}
			d.tasks.mu.Unlock()
			d.startTask(seed)
			cp := *existing
			return &cp, true, nil
		default:
			cp := *existing
			d.tasks.mu.Unlock()
			return &cp, false, nil
		}
	}
	t := &Task{
		SeedHashHex: seed, RootID: info.DefaultDownloadRootID,
		State:     TaskPending,
		CreatedAt: nowRFC3339(), UpdatedAt: nowRFC3339(),
		subs: make(map[chan Task]struct{}),
	}
	d.tasks.tasks[seed] = t
	if err := d.tasks.saveLocked(); err != nil {
		delete(d.tasks.tasks, seed)
		d.tasks.mu.Unlock()
		return nil, false, fmt.Errorf("任务落盘失败: %w", err)
	}
	d.tasks.mu.Unlock()
	d.startTask(seed)
	tasksCopy := *t
	return &tasksCopy, true, nil
}

func (d *Daemon) cancelTask(seed string) error {
	if d.tasks == nil {
		return fmt.Errorf("任务不存在")
	}
	d.tasks.mu.Lock()
	t, ok := d.tasks.tasks[seed]
	if !ok {
		d.tasks.mu.Unlock()
		return fmt.Errorf("任务不存在")
	}
	if t.State == TaskDone || t.State == TaskFailed || t.State == TaskCancelled {
		d.tasks.mu.Unlock()
		return nil
	}
	cancel := t.cancel
	t.State = TaskCancelled
	t.Detail = "已取消：停止新增采购，有界收尾中"
	d.tasks.publishLocked(t)
	d.tasks.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (d *Daemon) serveTaskEvents(w http.ResponseWriter, r *http.Request, seed string) {
	if d.store == nil || d.tasks == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan Task, 16)
	d.tasks.mu.Lock()
	t, exists := d.tasks.tasks[seed]
	if !exists {
		d.tasks.mu.Unlock()
		// Emit the persisted state once, then close.
		if persisted := d.loadPersistedTask(seed); persisted != nil {
			_ = json.NewEncoder(w).Encode(persisted)
		} else {
			http.Error(w, "任务不存在", http.StatusNotFound)
		}
		return
	}
	if t.subs == nil {
		t.subs = make(map[chan Task]struct{})
	}
	t.subs[ch] = struct{}{}
	current := *t
	d.tasks.mu.Unlock()
	defer func() {
		d.tasks.mu.Lock()
		if cur, ok := d.tasks.tasks[seed]; ok {
			delete(cur.subs, ch)
		}
		d.tasks.mu.Unlock()
	}()
	_ = writeSSE(w, current)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case updated := <-ch:
			_ = writeSSE(w, updated)
			flusher.Flush()
			if updated.State == TaskDone || updated.State == TaskFailed || updated.State == TaskCancelled {
				return
			}
		case <-time.After(30 * time.Second):
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, t Task) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

// loadPersistedTask reports persisted tasks (restored at startup) so a
// reconnecting CLI can query finished or interrupted work without a live run.
func (d *Daemon) loadPersistedTask(seed string) *Task {
	if d.tasks == nil {
		return nil
	}
	t, ok := d.tasks.get(seed)
	if !ok {
		return nil
	}
	return t
}
