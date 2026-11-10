package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
)

// handleBrowse lists subdirectories of an absolute path so the Web UI can
// offer a VSCode-like directory picker. It never returns file contents, only
// directory names. The daemon only listens on loopback; callers on this
// machine can already call the management API.
func (d *Daemon) handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path = "/"
	}
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		http.Error(w, "path 必须是绝对路径", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(cleaned)
	if err != nil {
		http.Error(w, "目录不可访问: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !info.IsDir() {
		http.Error(w, "不是目录", http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(cleaned)
	if err != nil {
		http.Error(w, "读取目录失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	dirs := []kmapi.BrowseEntry{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip symlinks that escape nowhere? List them; selection validates.
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
	writeJSON(w, kmapi.BrowseView{Path: cleaned, Parent: reqStr(parent), Dirs: dirs})
}
