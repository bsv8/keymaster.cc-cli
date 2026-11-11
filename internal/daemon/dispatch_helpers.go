package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/collector"
)

var seedHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validSeedHash(s string) bool { return seedHashPattern.MatchString(strings.ToLower(s)) }

func browseDirs(path string) ([]kmapi.BrowseEntry, string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return nil, "", errf("path 必须是绝对路径")
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		return nil, "", errf("目录不可访问: %v", err)
	}
	if !info.IsDir() {
		return nil, "", errf("不是目录")
	}
	entries, err := os.ReadDir(cleaned)
	if err != nil {
		return nil, "", errf("读取目录失败: %v", err)
	}
	dirs := []kmapi.BrowseEntry{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dirs = append(dirs, kmapi.BrowseEntry{Name: e.Name(), Path: filepath.Join(cleaned, e.Name())})
		if len(dirs) >= 500 {
			break
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })
	parent := filepath.Dir(cleaned)
	if parent == cleaned {
		parent = ""
	}
	return dirs, parent, nil
}

func (d *Daemon) refreshCatalog(ctx context.Context, rootID string, force bool) (any, error) {
	if rootID == "" {
		type one struct {
			RootID string `json:"rootId"`
			Report any    `json:"report"`
		}
		out := []one{}
		for _, h := range d.roots.list() {
			if h.deleted || h.catalog == nil {
				continue
			}
			report, err := h.catalog.Refresh(ctx, force)
			if err != nil {
				return nil, err
			}
			out = append(out, one{RootID: h.root.ID, Report: report})
		}
		d.feedCollectors()
		return map[string]any{"refreshed": out}, nil
	}
	h, ok := d.roots.get(rootID)
	if !ok || h.catalog == nil {
		return nil, errf("售卖根不可用")
	}
	report, err := h.catalog.Refresh(ctx, force)
	if err != nil {
		return nil, err
	}
	d.feedCollectors()
	return report, nil
}

func (d *Daemon) collectorHistoryView() kmapi.CollectorHistory {
	merged := []kmapi.CollectorSubmission{}
	if d.collectors != nil {
		d.collectors.each(func(id string, svc *collector.Service) {
			st := svc.Status()
			hist, _ := st["history"].([]collector.Submission)
			if hist == nil {
				raw, _ := json.Marshal(st["history"])
				_ = json.Unmarshal(raw, &hist)
			}
			for _, h := range hist {
				merged = append(merged, kmapi.CollectorSubmission{CollectorID: id, ArchiveID: h.ArchiveID, At: h.At, Trigger: h.Trigger, Count: h.Count})
			}
		})
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].At > merged[j].At })
	if len(merged) > 100 {
		merged = merged[:100]
	}
	return kmapi.CollectorHistory{Submissions: merged}
}

func (d *Daemon) collectorSetDescription(ctx context.Context, seedHash, desc string) error {
	if d.collectors == nil {
		return errf("采集未配置")
	}
	var firstErr error
	d.collectors.each(func(_ string, svc *collector.Service) {
		if err := svc.SetDescription(seedHash, desc); err != nil && firstErr == nil {
			firstErr = err
		}
	})
	if firstErr != nil {
		return firstErr
	}
	d.publishCollector()
	return nil
}

func (d *Daemon) collectorApplyConfig(ctx context.Context, body kmapi.CollectorConfigRequest) error {
	d.settingsApplyMu.Lock()
	updated, err := d.store.UpdateSettings(body.Revision, func(next *appconfig.Settings) error {
		if body.Collectors != nil {
			next.Server.Collector.Collectors = collectorEntriesToApp(body.Collectors)
		}
		if body.EarlyTriggerCount != nil {
			next.Server.Collector.EarlyTriggerCount = *body.EarlyTriggerCount
		}
		if body.SubmitIntervalMs != nil {
			next.Server.Collector.SubmitIntervalMs = *body.SubmitIntervalMs
		}
		if body.RetryBaseMs != nil {
			next.Server.Collector.RetryBaseMs = *body.RetryBaseMs
		}
		if body.RetryCapMs != nil {
			next.Server.Collector.RetryCapMs = *body.RetryCapMs
		}
		return nil
	})
	if err != nil {
		d.settingsApplyMu.Unlock()
		return err
	}
	d.rebuildCollectors()
	d.feedCollectors()
	d.settingsApplyMu.Unlock()
	d.publishSettings(updated.Revision)
	d.publishCollector()
	return nil
}

func (d *Daemon) collectorFlushView(ctx context.Context) kmapi.CollectorFlushResponse {
	triggered := []string{}
	if d.collectors != nil {
		d.collectors.each(func(id string, svc *collector.Service) {
			if svc.Flush(ctx) {
				triggered = append(triggered, id)
			}
		})
	}
	d.publishCollector()
	return kmapi.CollectorFlushResponse{Triggered: triggered}
}

func (d *Daemon) collectorRetryView(ctx context.Context) kmapi.CollectorRetryResponse {
	retried := []string{}
	if d.collectors != nil {
		d.collectors.each(func(id string, svc *collector.Service) {
			if svc.RetryNow(ctx) {
				retried = append(retried, id)
			}
		})
	}
	d.publishCollector()
	return kmapi.CollectorRetryResponse{Retried: retried}
}

func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
