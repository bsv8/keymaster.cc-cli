package daemon

import (
	"encoding/json"
	"net/http"
	"sort"
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
	type fileView struct {
		RootID     string  `json:"rootId"`
		Path       string  `json:"path"`
		Name       string  `json:"name"`
		Size       *uint64 `json:"size"`
		SeedHash   string  `json:"seedHash"`
		Blocks     *uint64 `json:"blocks"`
		SeedPrice  string  `json:"seedPrice"`
		BlockPrice string  `json:"blockPrice"`
		Override   bool    `json:"override"`
		State      string  `json:"state"`
		Error      string  `json:"error,omitempty"`
	}
	views := []fileView{}
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
			views = append(views, fileView{
				RootID: h.root.ID, Path: e.RelativePath, Name: e.Name,
				Size: size, SeedHash: e.SeedHashHex, Blocks: blocks,
				SeedPrice: seedPrice, BlockPrice: blockPrice, Override: override,
				State: e.State, Error: e.Error,
			})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].RootID != views[j].RootID {
			return views[i].RootID < views[j].RootID
		}
		return views[i].Path < views[j].Path
	})
	writeJSON(w, map[string]any{"files": views})
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
	var body struct {
		RootID string `json:"rootId"`
		Force  bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.RootID == "" {
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
			report, err := h.catalog.Refresh(r.Context(), body.Force)
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
	h, ok := d.roots.get(body.RootID)
	if !ok || h.catalog == nil {
		http.Error(w, "售卖根不可用", http.StatusNotFound)
		return
	}
	report, err := h.catalog.Refresh(r.Context(), body.Force)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
	d.feedCollectors()
}
