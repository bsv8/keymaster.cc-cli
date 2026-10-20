// Package pricing stores per-file seller prices with a server default
// fallback. Directory batch operations only write per-file entries; there is
// no directory price property and no inheritance.
package pricing

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Version is the pricing schema version.
const Version = 1

var satsPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// FilePrice is one explicit file price.
type FilePrice struct {
	SeedPriceSatoshis      string `json:"seedPriceSatoshis"`
	FullBlockPriceSatoshis string `json:"fullBlockPriceSatoshis"`
	UpdatedAt              string `json:"updatedAt"`
}

type state struct {
	Version                       int                  `json:"version"`
	DefaultSeedPriceSatoshis      string               `json:"defaultSeedPriceSatoshis"`
	DefaultFullBlockPriceSatoshis string               `json:"defaultFullBlockPriceSatoshis"`
	Files                         map[string]FilePrice `json:"files"`
}

// Pricing is the persisted pricing table for one server. HTTP handlers and
// seller quotes share one instance, so every access is guarded by mu.
type Pricing struct {
	mu    sync.RWMutex
	path  string
	state state
}

// Open loads the pricing table at path or creates it with the given defaults.
func Open(path, defaultSeedPrice, defaultBlockPrice string) (*Pricing, error) {
	if path == "" {
		return nil, errors.New("pricing: path is required")
	}
	if !validSats(defaultSeedPrice) || !validSats(defaultBlockPrice) {
		return nil, errors.New("pricing: defaults must be canonical sats")
	}
	table := &Pricing{path: path}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &table.state); err != nil {
			return nil, fmt.Errorf("pricing: invalid pricing file: %w", err)
		}
		if table.state.Version != Version {
			return nil, fmt.Errorf("pricing: unsupported version %d", table.state.Version)
		}
		if !validSats(table.state.DefaultSeedPriceSatoshis) || !validSats(table.state.DefaultFullBlockPriceSatoshis) {
			return nil, errors.New("pricing: invalid stored defaults")
		}
		if table.state.Files == nil {
			table.state.Files = map[string]FilePrice{}
		}
		// config.json is the source of truth for the server defaults; stored
		// per-file overrides remain. Persist the effective defaults so the
		// file and the running configuration cannot drift.
		if table.state.DefaultSeedPriceSatoshis != defaultSeedPrice || table.state.DefaultFullBlockPriceSatoshis != defaultBlockPrice {
			table.state.DefaultSeedPriceSatoshis = defaultSeedPrice
			table.state.DefaultFullBlockPriceSatoshis = defaultBlockPrice
			if err := table.Save(); err != nil {
				return nil, err
			}
		}
		return table, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pricing: read pricing: %w", err)
	}
	table.state = state{
		Version:                       Version,
		DefaultSeedPriceSatoshis:      defaultSeedPrice,
		DefaultFullBlockPriceSatoshis: defaultBlockPrice,
		Files:                         map[string]FilePrice{},
	}
	if err := table.Save(); err != nil {
		return nil, err
	}
	return table, nil
}

// Save writes the table atomically with 0600 permissions.
func (p *Pricing) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked()
}

func (p *Pricing) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return fmt.Errorf("pricing: create directory: %w", err)
	}
	encoded, err := json.MarshalIndent(p.state, "", "  ")
	if err != nil {
		return fmt.Errorf("pricing: encode: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(p.path), ".pricing-*.tmp")
	if err != nil {
		return fmt.Errorf("pricing: create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, p.path); err != nil {
		return fmt.Errorf("pricing: replace: %w", err)
	}
	return nil
}

// Defaults returns the current server defaults.
func (p *Pricing) Defaults() (string, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.state.DefaultSeedPriceSatoshis, p.state.DefaultFullBlockPriceSatoshis
}

// SetDefaults updates the server default prices.
func (p *Pricing) SetDefaults(seedPrice, blockPrice string) error {
	if !validSats(seedPrice) || !validSats(blockPrice) {
		return errors.New("pricing: defaults must be canonical sats")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.DefaultSeedPriceSatoshis = seedPrice
	p.state.DefaultFullBlockPriceSatoshis = blockPrice
	return p.saveLocked()
}

// Effective returns the price for a file: its explicit price when set,
// otherwise the current server default.
func (p *Pricing) Effective(relativePath string) (seedPrice, blockPrice string, override bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if price, ok := p.state.Files[relativePath]; ok {
		return price.SeedPriceSatoshis, price.FullBlockPriceSatoshis, true
	}
	return p.state.DefaultSeedPriceSatoshis, p.state.DefaultFullBlockPriceSatoshis, false
}

// SetFile sets one file's explicit prices.
func (p *Pricing) SetFile(relativePath, seedPrice, blockPrice string) error {
	if relativePath == "" || filepath.IsAbs(relativePath) || filepath.Clean(relativePath) == ".." {
		return fmt.Errorf("pricing: invalid path %q", relativePath)
	}
	if !validSats(seedPrice) || !validSats(blockPrice) {
		return errors.New("pricing: prices must be canonical sats")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state.Files[filepath.ToSlash(relativePath)] = FilePrice{
		SeedPriceSatoshis:      seedPrice,
		FullBlockPriceSatoshis: blockPrice,
		UpdatedAt:              time.Now().UTC().Format(time.RFC3339Nano),
	}
	return p.saveLocked()
}

// ClearFile removes one file's explicit prices (falling back to the default).
func (p *Pricing) ClearFile(relativePath string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := filepath.ToSlash(relativePath)
	if _, ok := p.state.Files[key]; !ok {
		return nil
	}
	delete(p.state.Files, key)
	return p.saveLocked()
}

// BatchAll sets the same explicit price for every listed file.
func (p *Pricing) BatchAll(paths []string, seedPrice, blockPrice string) error {
	return p.batch(paths, seedPrice, blockPrice, false)
}

// BatchUnpriced sets explicit prices only for files that have no explicit
// price yet.
func (p *Pricing) BatchUnpriced(paths []string, seedPrice, blockPrice string) error {
	return p.batch(paths, seedPrice, blockPrice, true)
}

func (p *Pricing) batch(paths []string, seedPrice, blockPrice string, onlyUnpriced bool) error {
	if !validSats(seedPrice) || !validSats(blockPrice) {
		return errors.New("pricing: prices must be canonical sats")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	for _, path := range paths {
		key := filepath.ToSlash(path)
		if key == "" {
			continue
		}
		if _, exists := p.state.Files[key]; onlyUnpriced && exists {
			continue
		}
		p.state.Files[key] = FilePrice{SeedPriceSatoshis: seedPrice, FullBlockPriceSatoshis: blockPrice, UpdatedAt: updatedAt}
	}
	return p.saveLocked()
}

// Files returns the explicit file prices sorted by path.
func (p *Pricing) Files() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	paths := make([]string, 0, len(p.state.Files))
	for path := range p.state.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Prune removes explicit prices whose path is not listed (deleted files).
func (p *Pricing) Prune(existing []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	keep := make(map[string]bool, len(existing))
	for _, path := range existing {
		keep[filepath.ToSlash(path)] = true
	}
	changed := false
	for path := range p.state.Files {
		if !keep[path] {
			delete(p.state.Files, path)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return p.saveLocked()
}

func validSats(value string) bool { return satsPattern.MatchString(value) }
