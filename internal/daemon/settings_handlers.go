package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
)

// handleSettings serves the ordinary settings (settings.json) with revision
// concurrency. Identity lives in info.json and is never modified here.
// Request/response DTOs are the protocol types; partial updates keep pointer
// semantics (omitted fields untouched, explicit zeros validated).
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
		writeJSON(w, settingsDTO(settings))
	case http.MethodPut:
		if d.isStopping() {
			http.Error(w, "后台停止中", http.StatusServiceUnavailable)
			return
		}
		var body kmapi.SettingsUpdate
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
		writeJSON(w, settingsDTO(updated))
		d.publishSettings(updated.Revision)
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
	var body kmapi.RevisionRequest
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
	writeJSON(w, settingsDTO(updated))
	d.publishSettings(updated.Revision)
}
