package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
)

var seedHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	d.mu.Lock()
	state := d.state
	hasStore := d.store != nil
	publicKey := d.publicKey
	address := d.address
	d.mu.Unlock()
	if !hasStore {
		writeJSON(w, kmapi.Status{
			Initialized: false, State: string(StateUninitialized),
			Product: DaemonProduct, APIVersion: DaemonAPIVersion,
		})
		return
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, kmapi.Status{
		Initialized: true,
		State:       string(state),
		Network:     reqStr(info.BsvNetwork),
		Locked:      optBool(state != StateUnlocked, true),
		Address:     reqStr(address),
		PublicKey:   reqStr(publicKey),
		Product:     DaemonProduct,
		APIVersion:  DaemonAPIVersion,
	})
}

func (d *Daemon) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	d.mu.Lock()
	state := d.state
	hasStore := d.store != nil
	address := d.address
	d.mu.Unlock()
	if !hasStore {
		writeJSON(w, map[string]any{"initialized": false, "state": StateUninitialized})
		return
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := kmapi.Info{
		Connected: true,
		Network:   info.BsvNetwork,
		Locked:    state != StateUnlocked,
		Address:   address,
	}
	bal, cached, queryErr := d.queryBalance(r.Context())
	if queryErr != nil {
		out.Error = optStr(queryErr.Error())
		if cached != nil {
			out.TotalSats = optU64(cached.Total, true)
			out.AvailableSats = optU64(cached.Available, true)
			out.AllocatedSats = optU64(cached.Allocated, true)
			out.QueryTime = reqStr(cached.QueryTime)
			out.Cached = optBool(true, true)
		} else {
			out.Cached = optBool(false, true)
		}
	} else {
		out.TotalSats = optU64(bal.Total, true)
		out.AvailableSats = optU64(bal.Available, true)
		out.AllocatedSats = optU64(bal.Allocated, true)
		out.QueryTime = reqStr(bal.QueryTime)
		out.Cached = optBool(false, true)
	}
	writeJSON(w, out)
}

func (d *Daemon) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var body kmapi.UnlockRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// Clear the decoded password copy as soon as possible; the request body
	// buffer itself is discarded by the server.
	defer func() {
		for i := range body.Password {
			_ = i
		}
		body.Password = ""
	}()
	if body.Password == "" {
		http.Error(w, "password is required", http.StatusBadRequest)
		return
	}
	if err := d.Unlock(body.Password); err != nil {
		if strings.Contains(err.Error(), "密码错误") || strings.Contains(err.Error(), "解锁失败") {
			http.Error(w, "解锁失败：密码错误", http.StatusUnauthorized)
			return
		}
		if strings.Contains(err.Error(), "尚未初始化") {
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, kmapi.Ok{Ok: true})
}

func (d *Daemon) handleRoots(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		http.Error(w, "尚未初始化，请先运行 km init", http.StatusPreconditionFailed)
		return
	}
	switch r.Method {
	case http.MethodGet:
		info, _, err := d.store.Snapshot()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		views := make([]kmapi.RootView, 0, len(info.Roots))
		for _, root := range info.Roots {
			accessible, errStr := true, ""
			if h, ok := d.roots.get(root.ID); ok {
				if h.err != "" {
					accessible, errStr = false, h.err
				} else if h.catalog == nil {
					accessible, errStr = false, "目录不可访问"
				}
			}
			views = append(views, kmapi.RootView{ID: root.ID, Path: root.Path, Accessible: accessible, Error: optStr(errStr)})
		}
		writeJSON(w, kmapi.RootsView{
			Roots: views, DefaultDownloadRootID: reqStr(info.DefaultDownloadRootID),
			Revision: info.Revision,
		})
	case http.MethodPost:
		if d.isStopping() {
			http.Error(w, "后台停止中", http.StatusServiceUnavailable)
			return
		}
		d.mu.Lock()
		locked := d.state != StateUnlocked
		d.mu.Unlock()
		if locked {
			http.Error(w, "后台已锁定，请先解锁", http.StatusLocked)
			return
		}
		var body kmapi.AddRootRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Path) == "" {
			http.Error(w, "path is required", http.StatusBadRequest)
			return
		}
		info, _, err := d.store.Snapshot()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		cleaned, err := appconfig.ValidateNewRoot(d.layout.Dir, body.Path, info.Roots)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// A re-added path must not force-close the old catalog while
		// accepted sessions are still delivering from it (deleted roots
		// stay open until their sessions drain). Drained handles are
		// reaped here; when live sessions remain, refuse with 409 until
		// they finish instead of breaking in-flight delivery.
		if d.roots.tryReapDeletedPath(cleaned) {
			http.Error(w, "该目录的旧售卖根仍有在途会话，待收尾后重试", http.StatusConflict)
			return
		}
		id, err := appconfig.NewRootID()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		updated, err := d.store.UpdateInfo(body.Revision, func(next *appconfig.Info) error {
			next.Roots = append(next.Roots, appconfig.Root{ID: id, Path: cleaned})
			// The first root becomes the default download root.
			if next.DefaultDownloadRootID == "" {
				next.DefaultDownloadRootID = id
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		// Open the new root for serving. Deleted roots at the same path
		// were already reaped above when drained, and their in-flight
		// count cannot grow (new sessions skip deleted roots), so the
		// new catalog never hits the old directory lock.
		_, settings, _ := d.store.Snapshot()
		d.roots.tryReapDeletedPath(cleaned)
		d.roots.mu.Lock()
		d.roots.roots[id] = d.roots.openOne(r.Context(), appconfig.Root{ID: id, Path: cleaned}, settings.Server.DefaultSeedPriceSatoshis, settings.Server.DefaultFullBlockPriceSatoshis)
		d.roots.mu.Unlock()
		d.feedCollectors()
		writeJSON(w, kmapi.AddRootResponse{ID: id, Path: cleaned, Revision: updated.Revision})
		d.publishRoots(id, updated.Revision)
	case http.MethodDelete:
		// DELETE /api/roots?id=<id>&revision=<rev> (exact path; the
		// trailing-slash route below shares the same helper).
		d.deleteRoot(w, r)
	default:
		methodNotAllowed(w)
	}
}

// deleteRoot removes a sale root from future selling. Real files, sessions
// and funding history are never deleted here.
func (d *Daemon) deleteRoot(w http.ResponseWriter, r *http.Request) {
	if d.store == nil {
		http.Error(w, "尚未初始化", http.StatusPreconditionFailed)
		return
	}
	if d.isStopping() {
		http.Error(w, "后台停止中", http.StatusServiceUnavailable)
		return
	}
	d.mu.Lock()
	locked := d.state != StateUnlocked
	d.mu.Unlock()
	if locked {
		http.Error(w, "后台已锁定，请先解锁", http.StatusLocked)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	var rev int64
	if _, err := fmt.Sscanf(r.URL.Query().Get("revision"), "%d", &rev); err != nil {
		// Revision is required for safe concurrent edits; missing means stale.
		http.Error(w, "revision is required", http.StatusBadRequest)
		return
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if info.DefaultDownloadRootID == id {
		http.Error(w, "删除默认根前必须另选默认根", http.StatusBadRequest)
		return
	}
	if active := d.tasks.activeRoots(); active[id] {
		http.Error(w, "该根有活动任务，请先完成或取消相关任务", http.StatusConflict)
		return
	}
	updated, err := d.store.UpdateInfo(rev, func(next *appconfig.Info) error {
		kept := next.Roots[:0]
		found := false
		for _, root := range next.Roots {
			if root.ID == id {
				found = true
				continue
			}
			kept = append(kept, root)
		}
		if !found {
			return errors.New("售卖根不存在")
		}
		next.Roots = append([]appconfig.Root(nil), kept...)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	// Stop serving new sales from this root; history (sessions, ledgers) stays.
	// Real files are never deleted here. The catalog stays open while
	// in-flight sessions bound to this root drain, then is reaped; idle
	// roots are closed right away so the path can be re-added.
	d.roots.mu.Lock()
	if h, ok := d.roots.roots[id]; ok {
		h.deleted = true
	}
	d.roots.mu.Unlock()
	d.roots.reapDeleted()
	writeJSON(w, kmapi.RevisionedOk{Ok: true, Revision: updated.Revision})
	d.publishRoots(id, updated.Revision)
}

func (d *Daemon) handleRootRoute(w http.ResponseWriter, r *http.Request) {
	// DELETE /api/roots/?id=<id>&revision=<rev> (trailing-slash alias of
	// DELETE /api/roots).
	if r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	d.deleteRoot(w, r)
}

func (d *Daemon) handleRootsDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
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
	var body kmapi.SetDefaultRootRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	updated, err := d.store.UpdateInfo(body.Revision, func(next *appconfig.Info) error {
		found := false
		for _, root := range next.Roots {
			if root.ID == body.ID {
				found = true
				break
			}
		}
		if !found {
			return errors.New("售卖根不存在")
		}
		next.DefaultDownloadRootID = body.ID
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, kmapi.RevisionedOk{Ok: true, Revision: updated.Revision})
	d.publishRoots(body.ID, updated.Revision)
}

func validSeedHash(s string) bool { return seedHashPattern.MatchString(strings.ToLower(s)) }
