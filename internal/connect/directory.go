package connect

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	whatwg "github.com/nlnwa/whatwg-url/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const DirectoryTTL = int64(600000)

type DirectoryURL struct {
	Display    apppublication.AppDisplay `json:"display"`
	URL        string                    `json:"url"`
	Origin     string                    `json:"origin"`
	VerifiedAt int64                     `json:"verifiedAt,omitempty"`
}
type DirectoryFiles struct {
	Display          apppublication.AppDisplay `json:"display"`
	Indexseedhashhex string                    `json:"indexseedhashhex"`
	VerifiedAt       int64                     `json:"verifiedAt,omitempty"`
}
type DirectorySelection struct {
	URL              string `json:"url,omitempty"`
	Indexseedhashhex string `json:"indexseedhashhex,omitempty"`
}
type DirectoryEntry struct {
	AppPublicKeyHex string              `json:"appPublicKeyHex"`
	Name            string              `json:"name,omitempty"`
	Description     string              `json:"description,omitempty"`
	Icon            string              `json:"icon,omitempty"`
	URLs            []DirectoryURL      `json:"urls"`
	Files           []DirectoryFiles    `json:"files"`
	Favorite        bool                `json:"favorite"`
	MarketListed    bool                `json:"marketListed"`
	Local           bool                `json:"local"`
	Selected        *DirectorySelection `json:"selected,omitempty"`
}
type DirectoryPending struct {
	PendingID        string `json:"pendingId"`
	URL              string `json:"url,omitempty"`
	Indexseedhashhex string `json:"indexseedhashhex,omitempty"`
	Name             string `json:"name,omitempty"`
	Description      string `json:"description,omitempty"`
	Icon             string `json:"icon,omitempty"`
	Error            string `json:"error,omitempty"`
}
type MarketCache struct {
	Entries               []DirectoryEntry `json:"entries"`
	LastSuccessfulFetchAt int64            `json:"lastSuccessfulFetchAt"`
}
type directoryFile struct {
	Version   int                           `json:"version"`
	Market    MarketCache                   `json:"market"`
	Local     map[string]DirectoryEntry     `json:"local"`
	Favorites map[string]bool               `json:"favorites"`
	Pending   map[string]DirectoryPending   `json:"pending"`
	Selection map[string]DirectorySelection `json:"selection"`
}
type MarketFetchCall struct {
	done chan struct{}
	err  error
}

func (c *MarketFetchCall) Done() <-chan struct{} { return c.done }
func (c *MarketFetchCall) Err() error            { return c.err }

type DirectoryStore struct {
	mu          sync.Mutex
	path        string
	state       directoryFile
	fetchCall   *MarketFetchCall
	marketError string
	loadError   error
}

func NewDirectoryStore(dir string) *DirectoryStore {
	return &DirectoryStore{path: filepath.Join(dir, "directory.json"), state: directoryFile{Version: 2, Market: MarketCache{Entries: []DirectoryEntry{}}, Local: map[string]DirectoryEntry{}, Favorites: map[string]bool{}, Pending: map[string]DirectoryPending{}, Selection: map[string]DirectorySelection{}}}
}

// CanonicalEntryURL 保留完整路径、查询和片段，来源仅作传输边界。
func CanonicalEntryURL(raw string) (string, string, error) {
	origin, err := ParseEntryOrigin(raw)
	if err != nil {
		return "", "", err
	}
	u, err := whatwg.Parse(raw)
	if err != nil {
		return "", "", err
	}
	return u.Href(false), origin, nil
}

// ValidateDirectorySnapshot 拒绝整个无效快照，不跳过条目或删字段掩盖差异。
func ValidateDirectorySnapshot(entries []DirectoryEntry) error {
	if entries == nil {
		return fmt.Errorf("market entries must be an array")
	}
	seen := map[string]bool{}
	for i, e := range entries {
		if _, err := apppublication.ValidateAppPubKey(e.AppPublicKeyHex); err != nil {
			return err
		}
		if seen[e.AppPublicKeyHex] {
			return fmt.Errorf("duplicate App public key")
		}
		seen[e.AppPublicKeyHex] = true
		if e.URLs == nil || e.Files == nil || len(e.URLs)+len(e.Files) == 0 {
			return fmt.Errorf("invalid App channels")
		}
		urls := map[string]bool{}
		seeds := map[string]bool{}
		for j, u := range e.URLs {
			if _, err := apppublication.ParseDisplay(apppublication.DisplayMap(u.Display)); err != nil {
				return err
			}
			full, origin, err := CanonicalEntryURL(u.URL)
			if err != nil || origin != u.Origin || urls[full] {
				return fmt.Errorf("invalid or duplicate URL entry")
			}
			urls[full] = true
			entries[i].URLs[j].URL = full
		}
		for _, f := range e.Files {
			if _, err := apppublication.ParseDisplay(apppublication.DisplayMap(f.Display)); err != nil {
				return err
			}
			if !isSeed(f.Indexseedhashhex) || seeds[f.Indexseedhashhex] {
				return fmt.Errorf("invalid or duplicate Files entry")
			}
			seeds[f.Indexseedhashhex] = true
		}
	}
	return nil
}
func isSeed(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *DirectoryStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.loadError = err
		return err
	}
	var f directoryFile
	err = json.Unmarshal(raw, &f)
	if err == nil && (f.Version != 2 || f.Local == nil || f.Pending == nil || f.Favorites == nil || f.Selection == nil) {
		err = fmt.Errorf("incompatible directory format")
	}
	if err == nil {
		err = ValidateDirectorySnapshot(f.Market.Entries)
		if f.Market.LastSuccessfulFetchAt < 0 {
			err = fmt.Errorf("invalid market timestamp")
		}
	}
	if err == nil {
		for key, e := range f.Local {
			if e.AppPublicKeyHex != key {
				err = fmt.Errorf("incompatible local App identity")
				break
			}
			if err = ValidateDirectorySnapshot([]DirectoryEntry{e}); err != nil {
				break
			}
		}
	}
	if err == nil {
		for key := range f.Favorites {
			if ApplicationKey(key) == "" {
				err = fmt.Errorf("incompatible favorite identity")
				break
			}
		}
	}
	if err == nil {
		for key, selection := range f.Selection {
			if ApplicationKey(key) == "" || (selection.URL == "") == (selection.Indexseedhashhex == "") {
				err = fmt.Errorf("invalid selected channel")
				break
			}
			if selection.URL != "" {
				full, _, e := CanonicalEntryURL(selection.URL)
				if e != nil || full != selection.URL {
					err = fmt.Errorf("invalid selected URL")
					break
				}
			} else if !isSeed(selection.Indexseedhashhex) {
				err = fmt.Errorf("invalid selected Files seed")
				break
			}
		}
	}
	if err == nil {
		for id, p := range f.Pending {
			if p.PendingID != id || id == "" || (p.URL == "") == (p.Indexseedhashhex == "") {
				err = fmt.Errorf("invalid pending identity")
				break
			}
			if p.URL != "" {
				full, _, e := CanonicalEntryURL(p.URL)
				if e != nil || full != p.URL {
					err = fmt.Errorf("invalid pending URL")
					break
				}
			} else if !isSeed(p.Indexseedhashhex) {
				err = fmt.Errorf("invalid pending seed")
				break
			}
		}
	}

	if err != nil {
		s.loadError = err
		return err
	}
	s.state = f
	return nil
}

// transactLocked 先保存完整候选状态，成功后才替换内存；失败保留所有旧状态。
func (s *DirectoryStore) transactLocked(change func(*directoryFile) error) error {
	if s.loadError != nil {
		return s.loadError
	}
	raw, _ := json.Marshal(s.state)
	var next directoryFile
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err := change(&next); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err = os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err = os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	s.state = next
	return nil
}
func (s *DirectoryStore) AddPending(p DirectoryPending) (DirectoryPending, error) {
	if (p.URL == "") == (p.Indexseedhashhex == "") {
		return p, fmt.Errorf("exactly one URL or seed is required")
	}
	if p.URL != "" {
		full, _, err := CanonicalEntryURL(p.URL)
		if err != nil {
			return p, err
		}
		p.URL = full
	} else if !isSeed(p.Indexseedhashhex) {
		return p, fmt.Errorf("invalid seed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.state.Pending {
		if old.URL == p.URL && old.Indexseedhashhex == p.Indexseedhashhex {
			return old, nil
		}
	}
	p.PendingID = "pending_" + rand.Text()
	err := s.transactLocked(func(f *directoryFile) error { f.Pending[p.PendingID] = p; return nil })
	return p, err
}
func (s *DirectoryStore) PendingError(id string, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactLocked(func(f *directoryFile) error {
		p, ok := f.Pending[id]
		if !ok {
			return fmt.Errorf("pending item not found")
		}
		p.Error = cause.Error()
		f.Pending[id] = p
		return nil
	})
}

// Resolve 只接受宿主已经验证的身份。目录中的预期公钥不能调用本方法授予身份。
func (s *DirectoryStore) Resolve(id string, e DirectoryEntry) error {
	if err := ValidateDirectorySnapshot([]DirectoryEntry{e}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactLocked(func(f *directoryFile) error {
		if old, ok := f.Local[e.AppPublicKeyHex]; ok {
			e = mergeDirectory(old, e)
		}
		e.Local = true
		f.Local[e.AppPublicKeyHex] = e
		if id != "" {
			delete(f.Pending, id)
		}
		return nil
	})
}
func mergeDirectory(a, b DirectoryEntry) DirectoryEntry {
	seenURL := map[string]bool{}
	for _, u := range a.URLs {
		seenURL[u.URL] = true
	}
	for _, u := range b.URLs {
		for i, old := range a.URLs {
			if old.URL == u.URL {
				a.URLs[i] = u
			}
		}
		if !seenURL[u.URL] {
			a.URLs = append(a.URLs, u)
			seenURL[u.URL] = true
		}
	}
	seenSeed := map[string]bool{}
	for _, f := range a.Files {
		seenSeed[f.Indexseedhashhex] = true
	}
	for _, f := range b.Files {
		if !seenSeed[f.Indexseedhashhex] {
			a.Files = append(a.Files, f)
			seenSeed[f.Indexseedhashhex] = true
		}
	}
	if a.Name == "" {
		a.Name = b.Name
	}
	if a.Description == "" {
		a.Description = b.Description
	}
	if a.Icon == "" {
		a.Icon = b.Icon
	}
	return a
}
func (s *DirectoryStore) listLocked() []DirectoryEntry {
	out := []DirectoryEntry{}
	seen := map[string]bool{}
	add := func(e DirectoryEntry, listed bool) {
		key := e.AppPublicKeyHex
		if local, ok := s.state.Local[key]; ok {
			e = mergeDirectory(local, e)
			e.Local = true
		}
		e.MarketListed = listed
		e.Favorite = s.state.Favorites[key]
		if selected, ok := s.state.Selection[key]; ok {
			copy := selected
			e.Selected = &copy
		}
		// Display one complete signed channel, retaining the user's selected entry.
		var display apppublication.AppDisplay
		if len(e.URLs) > 0 {
			first := e.URLs[0]
			for _, u := range e.URLs {
				if u.URL < first.URL {
					first = u
				}
			}
			display = first.Display
		} else if len(e.Files) > 0 {
			first := e.Files[0]
			for _, f := range e.Files {
				if f.Indexseedhashhex < first.Indexseedhashhex {
					first = f
				}
			}
			display = first.Display
		}
		if e.Selected != nil {
			for _, u := range e.URLs {
				if e.Selected.URL == u.URL {
					display = u.Display
				}
			}
			for _, f := range e.Files {
				if e.Selected.Indexseedhashhex == f.Indexseedhashhex {
					display = f.Display
				}
			}
		}
		e.Name, e.Description, e.Icon = display.Name, display.Description, display.Icon
		out = append(out, e)
		seen[key] = true
	}
	for _, e := range s.state.Market.Entries {
		add(e, true)
	}
	keys := []string{}
	for k := range s.state.Local {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(s.state.Local[k], false)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Favorite && !out[j].Favorite })
	return out
}
func (s *DirectoryStore) List() []DirectoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}
func (s *DirectoryStore) View() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := []DirectoryPending{}
	for _, p := range s.state.Pending {
		pending = append(pending, p)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].PendingID < pending[j].PendingID })
	out := map[string]any{"entries": s.listLocked(), "pending": pending}
	if s.loadError != nil {
		out["marketError"] = s.loadError.Error()
	} else if s.marketError != "" {
		out["marketError"] = s.marketError
	}
	return out
}
func (s *DirectoryStore) Remove(key, id string) error {
	if (key == "") == (id == "") {
		return fmt.Errorf("exactly one App public key or pending ID is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactLocked(func(f *directoryFile) error {
		if id != "" {
			if _, ok := f.Pending[id]; !ok {
				return fmt.Errorf("pending item not found")
			}
			delete(f.Pending, id)
			return nil
		}
		if f.Favorites[key] {
			return fmt.Errorf("unfavorite before removing")
		}
		if _, ok := f.Local[key]; !ok {
			return fmt.Errorf("local item not found")
		}
		delete(f.Local, key)
		delete(f.Selection, key)
		return nil
	})
}
func (s *DirectoryStore) Favorite(key string, value bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *DirectoryEntry
	for _, e := range s.listLocked() {
		if e.AppPublicKeyHex == key {
			copy := e
			found = &copy
			break
		}
	}
	if found == nil {
		return fmt.Errorf("unknown App public key")
	}
	return s.transactLocked(func(f *directoryFile) error {
		if value {
			f.Favorites[key] = true
			copy := *found
			copy.Local = true
			f.Local[key] = copy
		} else {
			delete(f.Favorites, key)
		}
		return nil
	})
}
func (s *DirectoryStore) Select(key string, selection DirectorySelection) error {
	if (selection.URL == "") == (selection.Indexseedhashhex == "") {
		return fmt.Errorf("exactly one channel is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := false
	var selectedApp DirectoryEntry
	for _, e := range s.listLocked() {
		if e.AppPublicKeyHex == key {
			selectedApp = e
			for _, u := range e.URLs {
				valid = valid || selection.URL != "" && u.URL == selection.URL
			}
			for _, f := range e.Files {
				valid = valid || selection.Indexseedhashhex != "" && f.Indexseedhashhex == selection.Indexseedhashhex
			}
		}
	}
	if !valid {
		return fmt.Errorf("selected channel not found")
	}
	return s.transactLocked(func(f *directoryFile) error {
		// 用户明确选择的入口也保存本地快照；刷新/下架不能使选择丢失。
		f.Local[key] = selectedApp
		f.Selection[key] = selection
		return nil
	})
}
func (s *DirectoryStore) ClaimMarketFetch(now int64) (bool, *MarketFetchCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadError != nil {
		return false, nil
	}
	if s.state.Market.LastSuccessfulFetchAt != 0 && now-s.state.Market.LastSuccessfulFetchAt < DirectoryTTL {
		return false, nil
	}
	if s.fetchCall != nil {
		return false, s.fetchCall
	}
	s.fetchCall = &MarketFetchCall{done: make(chan struct{})}
	return true, s.fetchCall
}
func (s *DirectoryStore) CompleteMarketFetch(entries []DirectoryEntry, at int64, cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := s.fetchCall
	if call == nil {
		return fmt.Errorf("no market fetch in progress")
	}
	defer close(call.done)
	s.fetchCall = nil
	if cause == nil {
		cause = ValidateDirectorySnapshot(entries)
	}
	if cause == nil {
		cause = s.transactLocked(func(f *directoryFile) error {
			f.Market = MarketCache{Entries: entries, LastSuccessfulFetchAt: at}
			return nil
		})
	}
	call.err = cause
	s.marketError = ""
	if cause != nil {
		s.marketError = cause.Error()
	}
	return cause
}
func (s *DirectoryStore) GetCache() (MarketCache, map[string]DirectoryEntry, map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Market, s.state.Local, s.state.Favorites
}

// ResolveURL 在钱包实际验证本次启动后才把匹配的 pending URL 归并。
func (s *DirectoryStore) ResolveURL(url, key string, display apppublication.AppDisplay) error {
	full, origin, err := CanonicalEntryURL(url)
	if err != nil {
		return err
	}
	s.mu.Lock()
	ids := []DirectoryPending{}
	for _, p := range s.state.Pending {
		if p.URL == full {
			ids = append(ids, p)
		}
	}
	s.mu.Unlock()
	for _, p := range ids {
		if err := s.Resolve(p.PendingID, DirectoryEntry{AppPublicKeyHex: key, Name: display.Name, Description: display.Description, Icon: display.Icon, URLs: []DirectoryURL{{URL: full, Origin: origin, Display: display}}, Files: []DirectoryFiles{}}); err != nil {
			return err
		}
	}
	return nil
}

// ResolveSeed 仅由安装器在整包验证完成后调用，失败下载不得认领 App 公钥。
func (s *DirectoryStore) ResolveSeed(seed, key string, display apppublication.AppDisplay) error {
	s.mu.Lock()
	pending := []DirectoryPending{}
	for _, p := range s.state.Pending {
		if p.Indexseedhashhex == seed {
			pending = append(pending, p)
		}
	}
	s.mu.Unlock()
	for _, p := range pending {
		if err := s.Resolve(p.PendingID, DirectoryEntry{AppPublicKeyHex: key, Name: display.Name, Description: display.Description, Icon: display.Icon, URLs: []DirectoryURL{}, Files: []DirectoryFiles{{Indexseedhashhex: seed, Display: display}}}); err != nil {
			return err
		}
	}
	return nil
}
