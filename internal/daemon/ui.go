package daemon

import (
	"io/fs"
	"net/http"

	kmweb "github.com/bsv8/keymaster.cc-cli/web"
)

// handleUI serves the embedded React build (web/dist). The placeholder is
// only a fallback when the frontend was not built; production binaries always
// carry the built assets and need no Node.js at runtime.
func (d *Daemon) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	sub, err := fs.Sub(kmweb.Dist, "dist")
	if err != nil {
		http.Error(w, "web ui unavailable", http.StatusNotFound)
		return
	}
	// Single-page app: unknown non-API paths fall back to index.html, but
	// /api/* is never handled here (mux routes it first).
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		if _, err := fs.Stat(sub, stripLeadingSlash(r.URL.Path)); err != nil {
			r.URL.Path = "/"
		}
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}

func stripLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
