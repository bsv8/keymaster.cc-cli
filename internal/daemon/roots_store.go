package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/pricing"
)

type rootHandle struct {
	root    appconfig.Root
	catalog *catalog.Catalog
	pricing *pricing.Pricing
	err     string
	// deleted stops new quotes but keeps the catalog open for in-flight
	// sessions bound to this root. History and real files are never deleted.
	deleted bool
}

type rootsManager struct {
	mu     sync.Mutex
	layout datadir.Layout
	store  *appconfig.Store
	roots  map[string]*rootHandle
	// inflight counts live seller sessions bound to each root. A deleted
	// root keeps its catalog open until its sessions drain, then is reaped.
	inflight map[string]int
}

func newRootsManager(layout datadir.Layout, store *appconfig.Store) *rootsManager {
	return &rootsManager{layout: layout, store: store, roots: map[string]*rootHandle{}, inflight: map[string]int{}}
}

func (m *rootsManager) rootDir(id string) string {
	return filepath.Join(m.layout.RootsDir, id)
}

func (m *rootsManager) openOne(ctx context.Context, r appconfig.Root, seedPrice, blockPrice string) *rootHandle {
	h := &rootHandle{root: r}
	dir := m.rootDir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.err = err.Error()
		return h
	}
	cat, err := catalog.Open(ctx, r.Path, filepath.Join(dir, "catalog.sqlite"))
	if err != nil {
		h.err = err.Error()
		// Pricing can still open for display even when the directory is
		// inaccessible; the error is surfaced, never auto-deleted.
		if p, perr := pricing.Open(filepath.Join(dir, "pricing.json"), seedPrice, blockPrice); perr == nil {
			h.pricing = p
		}
		return h
	}
	h.catalog = cat
	// Refresh once so the admin sees real state, but a walk failure must not
	// take down the daemon.
	if _, err := cat.Refresh(ctx, false); err != nil {
		h.err = err.Error()
	}
	p, err := pricing.Open(filepath.Join(dir, "pricing.json"), seedPrice, blockPrice)
	if err != nil {
		if h.catalog != nil {
			h.catalog.Close()
			h.catalog = nil
		}
		h.err = err.Error()
		return h
	}
	h.pricing = p
	return h
}

func (m *rootsManager) openAll(ctx context.Context) error {
	info, settings, err := m.store.Snapshot()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for _, r := range info.Roots {
		h := m.openOne(ctx, r, settings.Server.DefaultSeedPriceSatoshis, settings.Server.DefaultFullBlockPriceSatoshis)
		m.roots[r.ID] = h
		if h.err != "" && firstErr == nil {
			firstErr = fmt.Errorf("root %s: %s", r.ID, h.err)
		}
	}
	return firstErr
}

func (m *rootsManager) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.roots {
		if h.catalog != nil {
			_ = h.catalog.Close()
		}
	}
	m.roots = map[string]*rootHandle{}
}

func (m *rootsManager) list() []rootHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]rootHandle, 0, len(m.roots))
	for _, h := range m.roots {
		out = append(out, *h)
	}
	return out
}

// refreshAll runs the same ordinary check as startup/SIGHUP/manual refresh
// on every root: only new or length-changed files are re-indexed. Directory
// access failures are recorded on the handle, never auto-deleted.
func (m *rootsManager) refreshAll(ctx context.Context) {
	m.reapDeleted()
	for _, h := range m.list() {
		if h.deleted || h.catalog == nil {
			continue
		}
		if _, err := h.catalog.Refresh(ctx, false); err != nil {
			m.mu.Lock()
			if live, ok := m.roots[h.root.ID]; ok {
				live.err = err.Error()
			}
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			if live, ok := m.roots[h.root.ID]; ok {
				live.err = ""
			}
			m.mu.Unlock()
		}
		if err := ctx.Err(); err != nil {
			return
		}
	}
}

// refreshOne re-checks a single root (e.g. after a download lands there).
func (m *rootsManager) refreshOne(ctx context.Context, id string) {
	h, ok := m.get(id)
	if !ok || h.deleted || h.catalog == nil {
		return
	}
	if _, err := h.catalog.Refresh(ctx, false); err != nil {
		m.mu.Lock()
		if live, ok := m.roots[id]; ok {
			live.err = err.Error()
		}
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		if live, ok := m.roots[id]; ok {
			live.err = ""
		}
		m.mu.Unlock()
	}
}

func (m *rootsManager) get(id string) (*rootHandle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.roots[id]
	return h, ok
}

// retain marks one more live seller session bound to id.
func (m *rootsManager) retain(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.roots[id]; ok {
		m.inflight[id]++
	}
}

// release drops one live session binding. A deleted root whose sessions
// have all drained is closed and removed here so its directory locks do
// not leak and the path can be added again.
func (m *rootsManager) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight[id] > 0 {
		m.inflight[id]--
	}
	h, ok := m.roots[id]
	if !ok || !h.deleted || m.inflight[id] > 0 {
		return
	}
	m.closeHandleLocked(h)
	delete(m.roots, id)
	delete(m.inflight, id)
}

// reapDeleted closes deleted roots with no live sessions. It runs on the
// periodic check and after every release; re-adding a path uses
// tryReapDeletedPath (which additionally refuses while sessions drain).
func (m *rootsManager) reapDeleted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.roots {
		if h.deleted && m.inflight[id] <= 0 {
			m.closeHandleLocked(h)
			delete(m.roots, id)
			delete(m.inflight, id)
		}
	}
}

// tryReapDeletedPath closes deleted roots at path that have no live
// sessions. It reports whether a deleted root at path still has live
// sessions bound: re-adding the path must wait for them to drain (or be
// refused with 409) and must never force-close a catalog that in-flight
// deliveries are still reading.
func (m *rootsManager) tryReapDeletedPath(canonicalPath string) (busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, h := range m.roots {
		if h.deleted && h.root.Path == canonicalPath {
			if m.inflight[id] > 0 {
				busy = true
				continue
			}
			m.closeHandleLocked(h)
			delete(m.roots, id)
			delete(m.inflight, id)
		}
	}
	return busy
}

func (m *rootsManager) closeHandleLocked(h *rootHandle) {
	if h.catalog != nil {
		_ = h.catalog.Close()
		h.catalog = nil
	}
}
