package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
)

// settingsUpdate is the PUT /api/settings body. Pointers distinguish
// "not provided" from zero/empty values; omitted fields are left untouched.
type settingsUpdate struct {
	Revision             int64   `json:"revision"`
	Port                 *int    `json:"port,omitempty"`
	Theme                *string `json:"theme,omitempty"`
	Language             *string `json:"language,omitempty"`
	WOCAPIKey            *string `json:"wocApiKey,omitempty"`
	FeeRateSatoshisPerKb *int64  `json:"feeRateSatoshisPerKb,omitempty"`
	Scheduler            *struct {
		MaxBlockPriceSatoshis *string `json:"maxBlockPriceSatoshis,omitempty"`
		PoolBlocks            *int    `json:"poolBlocks,omitempty"`
		TotalSeats            *int    `json:"totalSeats,omitempty"`
		FloatingSeats         *int    `json:"floatingSeats,omitempty"`
		RequestTimeoutMs      *int    `json:"requestTimeoutMs,omitempty"`
	} `json:"scheduler,omitempty"`
	Server *struct {
		DefaultSeedPriceSatoshis      *string   `json:"defaultSeedPriceSatoshis,omitempty"`
		DefaultFullBlockPriceSatoshis *string   `json:"defaultFullBlockPriceSatoshis,omitempty"`
		ArbiterPublicKeys             *[]string `json:"arbiterPublicKeys,omitempty"`
		Collector                     *struct {
			Collectors        *[]appconfig.CollectorEntry `json:"collectors,omitempty"`
			EarlyTriggerCount *int                        `json:"earlyTriggerCount,omitempty"`
			SubmitIntervalMs  *int64                      `json:"submitIntervalMs,omitempty"`
			RetryBaseMs       *int64                      `json:"retryBaseMs,omitempty"`
			RetryCapMs        *int64                      `json:"retryCapMs,omitempty"`
		} `json:"collector,omitempty"`
	} `json:"server,omitempty"`
}

func applySettingsUpdate(next *appconfig.Settings, body *settingsUpdate) error {
	if body.Port != nil {
		next.Port = *body.Port
	}
	if body.Theme != nil {
		next.Theme = *body.Theme
	}
	if body.Language != nil {
		next.Language = *body.Language
	}
	if body.WOCAPIKey != nil {
		next.WOCAPIKey = *body.WOCAPIKey
	}
	if body.FeeRateSatoshisPerKb != nil {
		next.FeeRateSatoshisPerKb = *body.FeeRateSatoshisPerKb
	}
	if body.Scheduler != nil {
		if body.Scheduler.MaxBlockPriceSatoshis != nil {
			next.Scheduler.MaxBlockPriceSatoshis = *body.Scheduler.MaxBlockPriceSatoshis
		}
		if body.Scheduler.PoolBlocks != nil {
			next.Scheduler.PoolBlocks = *body.Scheduler.PoolBlocks
		}
		if body.Scheduler.TotalSeats != nil {
			next.Scheduler.TotalSeats = *body.Scheduler.TotalSeats
		}
		if body.Scheduler.FloatingSeats != nil {
			next.Scheduler.FloatingSeats = *body.Scheduler.FloatingSeats
		}
		if body.Scheduler.RequestTimeoutMs != nil {
			next.Scheduler.RequestTimeoutMs = *body.Scheduler.RequestTimeoutMs
		}
	}
	if body.Server != nil {
		if body.Server.DefaultSeedPriceSatoshis != nil {
			next.Server.DefaultSeedPriceSatoshis = *body.Server.DefaultSeedPriceSatoshis
		}
		if body.Server.DefaultFullBlockPriceSatoshis != nil {
			next.Server.DefaultFullBlockPriceSatoshis = *body.Server.DefaultFullBlockPriceSatoshis
		}
		if body.Server.ArbiterPublicKeys != nil {
			next.Server.ArbiterPublicKeys = append([]string(nil), *body.Server.ArbiterPublicKeys...)
		}
		if body.Server.Collector != nil {
			if body.Server.Collector.Collectors != nil {
				next.Server.Collector.Collectors = append([]appconfig.CollectorEntry(nil), *body.Server.Collector.Collectors...)
			}
			if body.Server.Collector.EarlyTriggerCount != nil {
				next.Server.Collector.EarlyTriggerCount = *body.Server.Collector.EarlyTriggerCount
			}
			if body.Server.Collector.SubmitIntervalMs != nil {
				next.Server.Collector.SubmitIntervalMs = *body.Server.Collector.SubmitIntervalMs
			}
			if body.Server.Collector.RetryBaseMs != nil {
				next.Server.Collector.RetryBaseMs = *body.Server.Collector.RetryBaseMs
			}
			if body.Server.Collector.RetryCapMs != nil {
				next.Server.Collector.RetryCapMs = *body.Server.Collector.RetryCapMs
			}
		}
	}
	return nil
}

// handleSettings serves the ordinary settings (settings.json) with revision
// concurrency. Identity lives in info.json and is never modified here.
func (d *Daemon) handleSettings(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	switch r.Method {
	case http.MethodGet:
		_, settings, err := d.store.Snapshot()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, settings)
	case http.MethodPut:
		if d.isStopping() {
			http.Error(w, "后台停止中", http.StatusServiceUnavailable)
			return
		}
		var body settingsUpdate
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		d.settingsApplyMu.Lock()
		updated, err := d.store.UpdateSettings(body.Revision, func(next *appconfig.Settings) error {
			return applySettingsUpdate(next, &body)
		})
		if err != nil {
			d.settingsApplyMu.Unlock()
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// Keep per-root pricing caches in sync with new global defaults.
		for _, h := range d.roots.list() {
			if h.deleted || h.pricing == nil {
				continue
			}
			_ = h.pricing.SetDefaults(updated.Server.DefaultSeedPriceSatoshis, updated.Server.DefaultFullBlockPriceSatoshis)
		}
		d.rebuildCollectors()
		d.feedCollectors()
		d.settingsApplyMu.Unlock()
		writeJSON(w, updated)
	default:
		methodNotAllowed(w)
	}
}

func (d *Daemon) handleSettingsReset(w http.ResponseWriter, r *http.Request) {
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
		Revision int64 `json:"revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	d.settingsApplyMu.Lock()
	updated, err := d.store.ResetSettings(body.Revision)
	if err != nil {
		d.settingsApplyMu.Unlock()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	for _, h := range d.roots.list() {
		if h.deleted || h.pricing == nil {
			continue
		}
		_ = h.pricing.SetDefaults(updated.Server.DefaultSeedPriceSatoshis, updated.Server.DefaultFullBlockPriceSatoshis)
	}
	d.rebuildCollectors()
	d.feedCollectors()
	d.settingsApplyMu.Unlock()
	writeJSON(w, updated)
}
