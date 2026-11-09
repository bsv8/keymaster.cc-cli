package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/collector"
	"github.com/bsv8/keymaster.cc-cli/internal/sell"
)

// collectorsManager fans every submission out to every enabled BitFSCollector
// target, each with its own queue/state/history file.
type collectorsManager struct {
	mu     sync.Mutex
	layout interface {
		Dir() string
	}
	dir    string
	store  *appconfig.Store
	svcs   map[string]*collector.Service
	cancel []context.CancelFunc
	wg     sync.WaitGroup
	sign   collector.SignFunc
}

func newCollectorsManager(dir string, store *appconfig.Store, sign collector.SignFunc) *collectorsManager {
	return &collectorsManager{dir: dir, store: store, svcs: map[string]*collector.Service{}, sign: sign}
}

// collectorStateDir resolves a per-collector state file strictly inside
// server/collector. IDs are validated at the settings layer, but the join is
// still verified so a hostile or hand-edited settings file can never escape
// into config, wallet or business files (e.g. id="../../info").
func collectorStateDir(dataDir, id string) (string, error) {
	base, err := filepath.Abs(filepath.Join(dataDir, "server", "collector"))
	if err != nil {
		return "", err
	}
	candidate, err := filepath.Abs(filepath.Join(base, id+".json"))
	if err != nil {
		return "", err
	}
	if filepath.Dir(candidate) != base {
		return "", fmt.Errorf("collector id 越界: %q", id)
	}
	return candidate, nil
}

// rebuild (re)creates one Service per configured collector and restarts run
// loops. Stale loops are cancelled; in-flight evidence stays in per-collector
// state files.
func (m *collectorsManager) rebuild(ctx context.Context) error {
	_, settings, err := m.store.Snapshot()
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, cancel := range m.cancel {
		cancel()
	}
	m.cancel = nil
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	m.svcs = map[string]*collector.Service{}
	for _, e := range settings.Server.Collector.Collectors {
		statePath, err := collectorStateDir(m.dir, e.ID)
		if err != nil {
			m.mu.Unlock()
			return err
		}
		svc, err := collector.New(statePath, collector.Options{
			Endpoint:          e.Endpoint,
			Enabled:           e.Enabled,
			Sign:              m.sign,
			EarlyTriggerCount: settings.Server.Collector.EarlyTriggerCount,
			SubmitInterval:    time.Duration(settings.Server.Collector.SubmitIntervalMs) * time.Millisecond,
			RetryBase:         time.Duration(settings.Server.Collector.RetryBaseMs) * time.Millisecond,
			RetryCap:          time.Duration(settings.Server.Collector.RetryCapMs) * time.Millisecond,
		})
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("collector %s: %w", e.ID, err)
		}
		m.svcs[e.ID] = svc
		cctx, cancel := context.WithCancel(ctx)
		m.cancel = append(m.cancel, cancel)
		m.wg.Add(1)
		go func(s *collector.Service) {
			defer m.wg.Done()
			s.Run(cctx)
		}(svc)
	}
	m.mu.Unlock()
	return nil
}

func (m *collectorsManager) stop() {
	m.mu.Lock()
	for _, cancel := range m.cancel {
		cancel()
	}
	m.cancel = nil
	m.mu.Unlock()
	m.wg.Wait()
}

// feed queues ready entries into every collector service.
func (m *collectorsManager) feed(entries []*catalog.Entry, mediaType func(string) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, svc := range m.svcs {
		collector.FeedEntries(svc, entries, mediaType)
	}
}

// each runs fn for every service.
func (m *collectorsManager) each(fn func(id string, svc *collector.Service)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, svc := range m.svcs {
		fn(id, svc)
	}
}

func sellMediaType(name string) string { return sell.MediaTypeForFileName(name) }

// collectorSign signs a 32-byte collector digest with the unlocked wallet key.
// Locked daemons refuse so submissions never proceed without a key.
func (d *Daemon) collectorSign(digest []byte) ([]byte, string, error) {
	d.mu.Lock()
	key := append([]byte(nil), d.privateKey...)
	locked := d.state != StateUnlocked || len(key) == 0
	d.mu.Unlock()
	if locked {
		return nil, "", fmt.Errorf("collector: 后台已锁定")
	}
	defer clearBytes(key)
	sign, err := collector.HostPrivateKeySigner(key)
	if err != nil {
		return nil, "", err
	}
	return sign(digest)
}

// ensureCollectorsStarted builds per-collector services and run loops.
// Idempotent: settings updates call rebuildCollectors instead.
// It refuses to start while stopping and prefers the daemon lifecycle as
// parent so Close always drains collector loops.
func (d *Daemon) ensureCollectorsStarted() {
	if d.isStopping() {
		return
	}
	if d.collectors == nil {
		return
	}
	parent := d.LifecycleContext()
	if parent == nil {
		parent = context.Background()
	}
	_ = d.collectors.rebuild(parent)
}

func (d *Daemon) rebuildCollectors() {
	if d.isStopping() {
		return
	}
	if d.collectors == nil {
		return
	}
	parent := d.LifecycleContext()
	if parent == nil {
		parent = context.Background()
	}
	_ = d.collectors.rebuild(parent)
}

// feedCollectors queues ready catalog entries into every collector.
func (d *Daemon) feedCollectors() {
	if d.collectors == nil || d.roots == nil {
		return
	}
	for _, h := range d.roots.list() {
		if h.deleted || h.catalog == nil {
			continue
		}
		entries, err := h.catalog.List(context.Background())
		if err != nil {
			continue
		}
		d.collectors.feed(entries, sellMediaType)
	}
}
