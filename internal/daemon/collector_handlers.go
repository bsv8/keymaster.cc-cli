package daemon

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/collector"
)

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
	_, settings, err := d.store.Snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type one struct {
		ID             string `json:"id"`
		Name           string `json:"name,omitempty"`
		Endpoint       string `json:"endpoint,omitempty"`
		Enabled        bool   `json:"enabled"`
		PendingCount   int    `json:"pendingCount"`
		ConfirmedCount int    `json:"confirmedCount"`
		Inflight       bool   `json:"inflight"`
		LastError      string `json:"lastError,omitempty"`
		LastSuccessAt  *int64 `json:"lastSuccessAt"`
	}
	out := []one{}
	if d.collectors != nil {
		names := map[string]string{}
		for _, e := range settings.Server.Collector.Collectors {
			names[e.ID] = e.Name
		}
		var mu []one
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
					lastPtr = &n
				case float64:
					n64 := int64(n)
					lastPtr = &n64
				}
			}
			mu = append(mu, one{
				ID: id, Name: names[id], Endpoint: ep, Enabled: en,
				PendingCount: pc, ConfirmedCount: cc, Inflight: inf,
				LastError: le, LastSuccessAt: lastPtr,
			})
		})
		out = mu
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, map[string]any{
		"collectors": out,
		"intervals": map[string]any{
			"earlyTriggerCount": settings.Server.Collector.EarlyTriggerCount,
			"submitIntervalMs":  settings.Server.Collector.SubmitIntervalMs,
			"retryBaseMs":       settings.Server.Collector.RetryBaseMs,
			"retryCapMs":        settings.Server.Collector.RetryCapMs,
		},
		"officialEndpoint": appconfig.DefaultCollectorEndpoint,
	})
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
	type one struct {
		CollectorID string `json:"collectorId"`
		ArchiveID   string `json:"archiveId"`
		At          int64  `json:"at"`
		Trigger     string `json:"trigger"`
		Count       int    `json:"count"`
	}
	merged := []one{}
	d.collectors.each(func(id string, svc *collector.Service) {
		st := svc.Status()
		hist, _ := st["history"].([]collector.Submission)
		if hist == nil {
			// Status() builds history as []Submission; JSON round trip guard.
			raw, _ := json.Marshal(st["history"])
			_ = json.Unmarshal(raw, &hist)
		}
		for _, h := range hist {
			merged = append(merged, one{CollectorID: id, ArchiveID: h.ArchiveID, At: h.At, Trigger: h.Trigger, Count: h.Count})
		}
	})
	sort.Slice(merged, func(i, j int) bool { return merged[i].At > merged[j].At })
	if len(merged) > 100 {
		merged = merged[:100]
	}
	writeJSON(w, map[string]any{"submissions": merged})
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
	var body struct {
		SeedHash    string `json:"seedHash"`
		Description string `json:"description"`
	}
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
	writeJSON(w, map[string]any{"ok": true})
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
	var body struct {
		Revision          int64                       `json:"revision"`
		Collectors        *[]appconfig.CollectorEntry `json:"collectors,omitempty"`
		EarlyTriggerCount *int                        `json:"earlyTriggerCount,omitempty"`
		SubmitIntervalMs  *int64                      `json:"submitIntervalMs,omitempty"`
		RetryBaseMs       *int64                      `json:"retryBaseMs,omitempty"`
		RetryCapMs        *int64                      `json:"retryCapMs,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	d.settingsApplyMu.Lock()
	_, err := d.store.UpdateSettings(body.Revision, func(next *appconfig.Settings) error {
		if body.Collectors != nil {
			next.Server.Collector.Collectors = append([]appconfig.CollectorEntry(nil), *body.Collectors...)
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
	writeJSON(w, map[string]any{"ok": true})
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
	writeJSON(w, map[string]any{"triggered": triggered})
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
	writeJSON(w, map[string]any{"retried": retried})
}
