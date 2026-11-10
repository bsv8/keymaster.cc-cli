package daemon

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
)

func (d *Daemon) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	views := []kmapi.FileView{}
	handles := d.roots.list()
	for _, h := range handles {
		if h.deleted || h.catalog == nil || h.pricing == nil {
			continue
		}
		entries, err := h.catalog.List(r.Context())
		if err != nil {
			continue
		}
		for _, e := range entries {
			seedPrice, blockPrice, override := h.pricing.Effective(e.RelativePath)
			var blocks, size *uint64
			if e.State == "ready" {
				c := e.BlockCount
				blocks = &c
			}
			if e.Size > 0 {
				v := e.Size
				size = &v
			}
			views = append(views, kmapi.FileView{
				RootID: h.root.ID, Path: e.RelativePath, Name: e.Name,
				Size: size, SeedHash: e.SeedHashHex, Blocks: blocks,
				SeedPrice: seedPrice, BlockPrice: blockPrice, Override: override,
				State: e.State, Error: optStr(e.Error),
			})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].RootID != views[j].RootID {
			return views[i].RootID < views[j].RootID
		}
		return views[i].Path < views[j].Path
	})
	writeJSON(w, kmapi.FilesView{Files: views})
}

func (d *Daemon) handleCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	var body kmapi.CatalogRefreshRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	rootID := ""
	if body.RootID != nil {
		rootID = *body.RootID
	}
	force := false
	if body.Force != nil {
		force = *body.Force
	}
	if rootID == "" {
		// Refresh all roots serially (each catalog has its own queue).
		type one struct {
			RootID string `json:"rootId"`
			Report any    `json:"report"`
		}
		out := []one{}
		for _, h := range d.roots.list() {
			if h.deleted {
				continue
			}
			if h.catalog == nil {
				continue
			}
			report, err := h.catalog.Refresh(r.Context(), force)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out = append(out, one{RootID: h.root.ID, Report: report})
		}
		writeJSON(w, map[string]any{"refreshed": out})
		d.feedCollectors()
		return
	}
	h, ok := d.roots.get(rootID)
	if !ok || h.catalog == nil {
		http.Error(w, "售卖根不可用", http.StatusNotFound)
		return
	}
	report, err := h.catalog.Refresh(r.Context(), force)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
	d.feedCollectors()
}
