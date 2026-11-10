package daemon

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/collector"
)

// collectorStatusView 构建采集器状态视图，供 API 与事件发布共用。
func (d *Daemon) collectorStatusView() kmapi.CollectorStatus {
	_, settings, err := d.store.Snapshot()
	if err != nil {
		return kmapi.CollectorStatus{}
	}
	out := []kmapi.CollectorStatusEntry{}
	if d.collectors != nil {
		names := map[string]string{}
		for _, e := range settings.Server.Collector.Collectors {
			names[e.ID] = e.Name
		}
		var mu []kmapi.CollectorStatusEntry
		d.collectors.each(func(id string, svc *collector.Service) {
			st := svc.Status()
			pc, _ := st["pendingCount"].(int)
			cc, _ := st["confirmedCount"].(int)
			inf, _ := st["inflight"].(bool)
			le, _ := st["lastError"].(string)
			ep, _ := st["endpoint"].(string)
			en, _ := st["enabled"].(bool)
			var lastPtr *int64
			if v, ok := st["lastSuccessAt"]; ok && v != nil {
				switch n := v.(type) {
				case int64:
					lastPtr = optI64(n, true)
				case float64:
					n64 := int64(n)
					lastPtr = optI64(n64, true)
				}
			}
			mu = append(mu, kmapi.CollectorStatusEntry{
				ID: id, Name: optStr(names[id]), Endpoint: optStr(ep), Enabled: en,
				PendingCount: pc, ConfirmedCount: cc, Inflight: inf,
				LastError: optStr(le), LastSuccessAt: lastPtr,
			})
		})
		out = mu
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return kmapi.CollectorStatus{
		Collectors: out,
		Intervals: map[string]any{
			"earlyTriggerCount": settings.Server.Collector.EarlyTriggerCount,
			"submitIntervalMs":  settings.Server.Collector.SubmitIntervalMs,
			"retryBaseMs":       settings.Server.Collector.RetryBaseMs,
			"retryCapMs":        settings.Server.Collector.RetryCapMs,
		},
		OfficialEndpoint: reqStr(appconfig.DefaultCollectorEndpoint),
	}
}

// handleCollectorStatus aggregates per-collector queues, intervals and state.
func (d *Daemon) handleCollectorStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	if _, _, err := d.store.Snapshot(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, d.collectorStatusView())
}

// handleCollectorHistory lists submission operation history per collector,
// newest first, capped at 100.
func (d *Daemon) handleCollectorHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if d.store == nil || d.collectors == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	merged := []kmapi.CollectorSubmission{}
	d.collectors.each(func(id string, svc *collector.Service) {
		st := svc.Status()
		hist, _ := st["history"].([]collector.Submission)
		if hist == nil {
			// Status() builds history as []Submission; JSON round trip guard.
			raw, _ := json.Marshal(st["history"])
			_ = json.Unmarshal(raw, &hist)
		}
		for _, h := range hist {
			merged = append(merged, kmapi.CollectorSubmission{CollectorID: id, ArchiveID: h.ArchiveID, At: h.At, Trigger: h.Trigger, Count: h.Count})
		}
	})
	sort.Slice(merged, func(i, j int) bool { return merged[i].At > merged[j].At })
	if len(merged) > 100 {
		merged = merged[:100]
	}
	writeJSON(w, kmapi.CollectorHistory{Submissions: merged})
}

// handleCollectorDescription saves a file description (collector-owned store,
// never touches original content) and re-queues it on every collector.
func (d *Daemon) handleCollectorDescription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if d.collectors == nil {
		http.Error(w, "采集未配置", http.StatusNotFound)
		return
	}
	var body kmapi.CollectorDescriptionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	var firstErr error
	d.collectors.each(func(_ string, svc *collector.Service) {
		if err := svc.SetDescription(body.SeedHash, body.Description); err != nil && firstErr == nil {
			firstErr = err
		}
	})
	if firstErr != nil {
		http.Error(w, firstErr.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, kmapi.Ok{Ok: true})
	d.publishCollector()
}

// handleCollectorConfig replaces the collector target list and intervals
// through the unified settings path, then rebuilds services.
func (d *Daemon) handleCollectorConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	if d.isStopping() {
		http.Error(w, "后台停止中", http.StatusServiceUnavailable)
		return
	}
	var body kmapi.CollectorConfigRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
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
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	d.rebuildCollectors()
	d.feedCollectors()
	d.settingsApplyMu.Unlock()
	writeJSON(w, kmapi.Ok{Ok: true})
	d.publishSettings(updated.Revision)
	d.publishCollector()
}

// handleCollectorFlush triggers an immediate submit on every enabled collector.
func (d *Daemon) handleCollectorFlush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if d.collectors == nil {
		http.Error(w, "采集未配置", http.StatusNotFound)
		return
	}
	if d.isStopping() {
		http.Error(w, "后台停止中", http.StatusServiceUnavailable)
		return
	}
	triggered := []string{}
	d.collectors.each(func(id string, svc *collector.Service) {
		if svc.Flush(r.Context()) {
			triggered = append(triggered, id)
		}
	})
	writeJSON(w, kmapi.CollectorFlushResponse{Triggered: triggered})
	d.publishCollector()
}

// handleCollectorRetry re-sends in-flight batches byte-identical.
func (d *Daemon) handleCollectorRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if d.collectors == nil {
		http.Error(w, "采集未配置", http.StatusNotFound)
		return
	}
	retried := []string{}
	d.collectors.each(func(id string, svc *collector.Service) {
		if svc.RetryNow(r.Context()) {
			retried = append(retried, id)
		}
	})
	writeJSON(w, kmapi.CollectorRetryResponse{Retried: retried})
	d.publishCollector()
}
