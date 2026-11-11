package daemon

import (
	"context"
	"fmt"
	"time"

)

// handleTasks creates or reuses a download task. Duplicate submissions for the
// same seed never start a second purchase: the existing task is returned.
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
