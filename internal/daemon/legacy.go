package daemon

import (
	"net/http"
)

// registerLegacyDownloads keeps the old buyer execution management paths
// available without a token while the React UI takes over. The new task model
// (/api/tasks) is authoritative; these paths report the same tasks where
// feasible and otherwise instruct callers to use /api/tasks.
func registerLegacyDownloads(mux *http.ServeMux, d *Daemon) {
	mux.HandleFunc("/api/downloads", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		if d.store == nil {
			http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
			return
		}
		writeJSON(w, map[string]any{"tasks": d.tasks.list(), "hint": "use /api/tasks"})
	})
	mux.HandleFunc("/api/downloads/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "请使用 /api/tasks", http.StatusGone)
	})
}
