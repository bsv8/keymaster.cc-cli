package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
)

func (d *Daemon) handlePricingDefaults(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
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
	var body kmapi.PricingDefaultsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	updated, err := func() (appconfig.Settings, error) {
		d.settingsApplyMu.Lock()
		defer d.settingsApplyMu.Unlock()
		return d.store.SetDefaultsPrices(body.Revision, body.SeedPrice, body.BlockPrice)
	}()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Sync per-root pricing caches to the new global defaults.
	d.settingsApplyMu.Lock()
	_, settings, _ := d.store.Snapshot()
	for _, h := range d.roots.list() {
		if h.deleted {
			continue
		}
		if h.pricing == nil {
			continue
		}
		_ = h.pricing.SetDefaults(settings.Server.DefaultSeedPriceSatoshis, settings.Server.DefaultFullBlockPriceSatoshis)
	}
	d.settingsApplyMu.Unlock()
	writeJSON(w, kmapi.PricingDefaultsResponse{SeedPrice: updated.Server.DefaultSeedPriceSatoshis, BlockPrice: updated.Server.DefaultFullBlockPriceSatoshis, Revision: updated.Revision})
	d.publishSettings(updated.Revision)
}

func (d *Daemon) handlePricingBatch(w http.ResponseWriter, r *http.Request) {
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
	var body kmapi.PricingBatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.RootID == "" {
		http.Error(w, "rootId is required", http.StatusBadRequest)
		return
	}
	h, ok := d.roots.get(body.RootID)
	if !ok || h.catalog == nil || h.pricing == nil {
		http.Error(w, "售卖根不可用", http.StatusNotFound)
		return
	}
	path := ""
	if body.Path != nil {
		path = *body.Path
	}
	entries, err := h.catalog.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		if path != "" && !(e.RelativePath == path || len(e.RelativePath) > len(path)+1 && e.RelativePath[:len(path)+1] == path+"/") {
			continue
		}
		paths = append(paths, e.RelativePath)
	}
	switch body.Mode {
	case "all":
		if err := h.pricing.BatchAll(paths, body.SeedPrice, body.BlockPrice); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	case "unpriced":
		if err := h.pricing.BatchUnpriced(paths, body.SeedPrice, body.BlockPrice); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		http.Error(w, "mode must be all or unpriced", http.StatusBadRequest)
		return
	}
	writeJSON(w, kmapi.PricingBatchResponse{Updated: len(paths)})
}

func (d *Daemon) handlePricingFile(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	if d.isStopping() {
		http.Error(w, "后台停止中", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body kmapi.PricingFileRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		h, ok := d.roots.get(body.RootID)
		if !ok || h.pricing == nil {
			http.Error(w, "售卖根不可用", http.StatusNotFound)
			return
		}
		if err := h.pricing.SetFile(body.Path, body.SeedPrice, body.BlockPrice); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, kmapi.PricingFileResponse{Updated: body.Path})
	case http.MethodDelete:
		rootID := r.URL.Query().Get("rootId")
		path := r.URL.Query().Get("path")
		if rootID == "" || path == "" {
			http.Error(w, "rootId and path are required", http.StatusBadRequest)
			return
		}
		h, ok := d.roots.get(rootID)
		if !ok || h.pricing == nil {
			http.Error(w, "售卖根不可用", http.StatusNotFound)
			return
		}
		if err := h.pricing.ClearFile(path); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, kmapi.PricingClearResponse{Cleared: path})
	default:
		methodNotAllowed(w)
	}
}
