package connect

import (
	"encoding/json"
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	whatwg "github.com/nlnwa/whatwg-url/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
)

// 手工来源使用固定标识；市场来源 ID（market-default、market_<随机值> 等）
// 由 Market 层分配并保证唯一，Connect 把来源 ID 当作关联键，不复验其拼写。
const SourceManual = "manual"

type DirectoryURL struct {
	PublicationID string                    `json:"publicationId,omitempty"`
	Display       apppublication.AppDisplay `json:"display"`
	URL           string                    `json:"url"`
	Origin        string                    `json:"origin"`
}
type DirectoryFiles struct {
	PublicationID    string                    `json:"publicationId,omitempty"`
	Display          apppublication.AppDisplay `json:"display"`
	Indexseedhashhex string                    `json:"indexseedhashhex"`
}
type DirectorySelection struct {
	URL              string `json:"url,omitempty"`
	Indexseedhashhex string `json:"indexseedhashhex,omitempty"`
}
type DirectoryEntry struct {
	PublicationID   string              `json:"publicationId,omitempty"`
	AppVersion      string              `json:"appVersion"`
	AppPublicKeyHex string              `json:"appPublicKeyHex"`
	Name            string              `json:"name,omitempty"`
	Description     string              `json:"description,omitempty"`
	Icon            string              `json:"icon,omitempty"`
	URLs            []DirectoryURL      `json:"urls"`
	Files           []DirectoryFiles    `json:"files"`
	Favorite        bool                `json:"favorite"`
	Selected        *DirectorySelection `json:"selected,omitempty"`
}

// SourceRecord 记录某来源已接受的整份发布；不拼接其他来源的字段。
// Sequence 是持久化提交序号：同版本不同内容按实际提交顺序判定，取序号
// 更大者；相同来源重放同一份已保存内容不改变序号。
type SourceRecord struct {
	SourceID    string         `json:"sourceId"`
	Publication DirectoryEntry `json:"publication"`
	Sequence    int64          `json:"sequence"`
}

type directoryFile struct {
	Version     int                                `json:"version"`
	Apps        map[string]DirectoryEntry          `json:"apps"`
	Sources     map[string]map[string]SourceRecord `json:"sources"`
	Favorites   map[string]bool                    `json:"favorites"`
	DeletedKeys []string                           `json:"deletedKeys"`
	Selection   map[string]DirectorySelection      `json:"selection"`
	// Sequence 为目录级单调递增提交序号；仅成功提交时推进。
	Sequence int64 `json:"sequence"`
}

type DirectoryStore struct {
	mu        sync.Mutex
	path      string
	state     directoryFile
	loadError error
}

func NewDirectoryStore(dir string) *DirectoryStore {
	return &DirectoryStore{path: filepath.Join(dir, "directory.json"), state: emptyDirectoryFile()}
}
func emptyDirectoryFile() directoryFile {
	return directoryFile{Version: 1, Apps: map[string]DirectoryEntry{}, Sources: map[string]map[string]SourceRecord{}, Favorites: map[string]bool{}, DeletedKeys: []string{}, Selection: map[string]DirectorySelection{}}
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
		if err := apppublication.ValidateAppVersion(e.AppVersion); err != nil {
			return err
		}
		if len(e.URLs) > 1 || len(e.Files) > 1 {
			return fmt.Errorf("multiple current publication channels")
		}
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
			if u.PublicationID != "" && !isSeed(u.PublicationID) {
				return fmt.Errorf("invalid URL publication digest")
			}
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
			if f.PublicationID != "" && !isSeed(f.PublicationID) {
				return fmt.Errorf("invalid Files publication digest")
			}
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
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
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
	if err == nil && (f.Version != 1 || f.Apps == nil || f.Sources == nil || f.Favorites == nil || f.Selection == nil || f.DeletedKeys == nil) {
		err = fmt.Errorf("incompatible directory format")
	}
	if err == nil {
		for key, e := range f.Apps {
			if e.AppPublicKeyHex != key {
				err = fmt.Errorf("incompatible App identity")
				break
			}
			if err = ValidateDirectorySnapshot([]DirectoryEntry{e}); err != nil {
				break
			}
		}
	}
	if err == nil {
		for key, perSource := range f.Sources {
			if ApplicationKey(key) == "" || perSource == nil {
				err = fmt.Errorf("incompatible App sources")
				break
			}
			if _, ok := f.Apps[key]; !ok {
				err = fmt.Errorf("source without current App entry")
				break
			}
			for sourceID, record := range perSource {
				if sourceID == "" {
					err = fmt.Errorf("empty source identity")
					break
				}
				if record.SourceID != sourceID || record.Publication.AppPublicKeyHex != key {
					err = fmt.Errorf("incompatible source record")
					break
				}
				if record.Sequence <= 0 {
					err = fmt.Errorf("invalid source sequence")
					break
				}
				if err = ValidateDirectorySnapshot([]DirectoryEntry{record.Publication}); err != nil {
					break
				}
			}
			if err != nil {
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
		for _, key := range f.DeletedKeys {
			if ApplicationKey(key) == "" {
				err = fmt.Errorf("incompatible deletion identity")
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

// SourceChange 是来源插件提交的两类变更：提交已验证发布或标记来源失效。
type SourceChange struct {
	Kind            string // "submit" | "invalidate"
	SourceID        string
	AppPublicKeyHex string
	AppVersion      string // submit
	Publication     DirectoryEntry
}

// SourceChangeResult 逐 App 返回结果；低版本只拒对应 App，不影响同批其他变更。
type SourceChangeResult struct {
	SourceID        string `json:"sourceId"`
	AppPublicKeyHex string `json:"appPublicKeyHex"`
	Status          string `json:"status"` // applied | unchanged | lower-version-rejected | failed
	Reason          string `json:"reason,omitempty"`
}

// samePublication 比较整份发布内容：版本、展示资料与渠道；不含收藏、选择等用户状态。
func samePublication(a, b DirectoryEntry) bool {
	return a.AppVersion == b.AppVersion && a.Name == b.Name && a.Description == b.Description && a.Icon == b.Icon &&
		slices.Equal(a.URLs, b.URLs) && slices.Equal(a.Files, b.Files)
}

// pickCurrent 从剩余有效来源取回一份完整发布：最高版本，同版本取最近一次成功应用。
func pickCurrent(perSource map[string]SourceRecord) (DirectoryEntry, string, bool) {
	var winner *SourceRecord
	for _, record := range perSource {
		if winner == nil {
			winner = &record
			continue
		}
		cmp := apppublication.CompareAppVersions(record.Publication.AppVersion, winner.Publication.AppVersion)
		if cmp > 0 || cmp == 0 && record.Sequence > winner.Sequence {
			winner = &record
		}
	}
	if winner == nil {
		return DirectoryEntry{}, "", false
	}
	return winner.Publication, winner.SourceID, true
}

func applySelection(entry *DirectoryEntry, selected DirectorySelection, ok bool) {
	entry.Selected = nil
	if ok && directoryHasSelection(*entry, selected) {
		copySel := selected
		entry.Selected = &copySel
	}
}

// ApplyBatch 是 Connect 的受限批次写入入口：仅来源插件（Market 同步与手工提交）调用。
// 提交已验证发布时高版本与后到的同版本整份覆盖；低版本只拒对应 App。来源失效只解除
// 指定来源；仍有其他有效来源时保留应用，最后一个来源失效后有收藏或使用数据则保留，
// 其余自动移除。hasUsage 无法确认时按“有使用”处理（保留）。
// 目录写盘失败时返回错误且结果无效：调用方不得据失效结果记账，须整轮作废重试。
func (s *DirectoryStore) ApplyBatch(changes []SourceChange, hasUsage func(string) bool) ([]SourceChangeResult, error) {
	results := make([]SourceChangeResult, 0, len(changes))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.transactLocked(func(f *directoryFile) error {
		for _, change := range changes {
			result := SourceChangeResult{SourceID: change.SourceID, AppPublicKeyHex: change.AppPublicKeyHex}
			switch change.Kind {
			case "submit":
				if change.SourceID == "" {
					result.Status, result.Reason = "failed", "empty source"
					break
				}
				publication := change.Publication
				if err := ValidateDirectorySnapshot([]DirectoryEntry{publication}); err != nil {
					result.Status, result.Reason = "failed", err.Error()
					break
				}
				if publication.AppPublicKeyHex != change.AppPublicKeyHex {
					result.Status, result.Reason = "failed", "publication key mismatch"
					break
				}
				if publication.AppVersion != change.AppVersion {
					result.Status, result.Reason = "failed", "version mismatch"
					break
				}
				if slices.Contains(f.DeletedKeys, change.AppPublicKeyHex) {
					result.Status, result.Reason = "failed", "deleted by user"
					break
				}
				perSource := f.Sources[change.AppPublicKeyHex]
				if old, ok := perSource[change.SourceID]; ok && samePublication(old.Publication, publication) {
					result.Status = "unchanged"
					break
				}
				if current, ok := f.Apps[change.AppPublicKeyHex]; ok &&
					apppublication.CompareAppVersions(publication.AppVersion, current.AppVersion) < 0 {
					result.Status, result.Reason = "lower-version-rejected",
						fmt.Sprintf("lower appVersion rejected: %s < %s", publication.AppVersion, current.AppVersion)
					break
				}
				if perSource == nil {
					perSource = map[string]SourceRecord{}
				}
				f.Sequence++
				perSource[change.SourceID] = SourceRecord{SourceID: change.SourceID, Publication: publication, Sequence: f.Sequence}
				f.Sources[change.AppPublicKeyHex] = perSource
				picked, _, _ := pickCurrent(perSource)
				previous := f.Apps[change.AppPublicKeyHex]
				next := picked
				next.Favorite = f.Favorites[change.AppPublicKeyHex]
				if previous.Selected != nil {
					applySelection(&next, *previous.Selected, true)
				}
				f.Apps[change.AppPublicKeyHex] = next
				if previous.AppVersion != "" && samePublication(previous, next) {
					result.Status = "unchanged"
				} else {
					result.Status = "applied"
				}
			case "invalidate":
				if change.SourceID == "" {
					result.Status, result.Reason = "failed", "empty source"
					break
				}
				perSource := f.Sources[change.AppPublicKeyHex]
				if _, ok := perSource[change.SourceID]; !ok {
					result.Status = "unchanged"
					break
				}
				delete(perSource, change.SourceID)
				if len(perSource) == 0 {
					delete(f.Sources, change.AppPublicKeyHex)
				} else {
					f.Sources[change.AppPublicKeyHex] = perSource
				}
				if len(perSource) > 0 {
					picked, _, _ := pickCurrent(perSource)
					previous := f.Apps[change.AppPublicKeyHex]
					next := picked
					next.Favorite = f.Favorites[change.AppPublicKeyHex]
					if previous.Selected != nil {
						applySelection(&next, *previous.Selected, true)
					}
					f.Apps[change.AppPublicKeyHex] = next
				} else {
					// 最后一个来源已失效：有收藏或使用数据保留条目（可能展示已失效入口），
					// 否则自动移除；hasUsage 无法确认时按有使用处理。
					used := f.Favorites[change.AppPublicKeyHex]
					if !used && hasUsage != nil {
						used = hasUsage(change.AppPublicKeyHex)
					}
					if !used {
						delete(f.Apps, change.AppPublicKeyHex)
						delete(f.Selection, change.AppPublicKeyHex)
					}
				}
				result.Status = "applied"
			default:
				result.Status, result.Reason = "failed", "unknown change kind"
			}
			results = append(results, result)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

func directoryHasSelection(entry DirectoryEntry, sel DirectorySelection) bool {
	if (sel.URL == "") == (sel.Indexseedhashhex == "") {
		return false
	}
	for _, channel := range entry.URLs {
		if sel.URL != "" && channel.URL == sel.URL {
			return true
		}
	}
	for _, channel := range entry.Files {
		if sel.Indexseedhashhex != "" && channel.Indexseedhashhex == sel.Indexseedhashhex {
			return true
		}
	}
	return false
}

// DeletedKeys 返回用户手动删除的 App 公钥快照；目录展示与页面补齐按其过滤。
func (s *DirectoryStore) DeletedKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.state.DeletedKeys...)
}

func (s *DirectoryStore) listLocked() []DirectoryEntry {
	out := []DirectoryEntry{}
	keys := make([]string, 0, len(s.state.Apps))
	for key := range s.state.Apps {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := s.state.Apps[key]
		entry.Favorite = s.state.Favorites[key]
		entry.Selected = nil
		if selected, ok := s.state.Selection[key]; ok && directoryHasSelection(entry, selected) {
			copySel := selected
			entry.Selected = &copySel
		}
		// 展示一个完整签名渠道的资料，保留用户选择的入口。
		var display apppublication.AppDisplay
		if len(entry.URLs) > 0 {
			first := entry.URLs[0]
			for _, u := range entry.URLs {
				if u.URL < first.URL {
					first = u
				}
			}
			display = first.Display
		} else if len(entry.Files) > 0 {
			first := entry.Files[0]
			for _, f := range entry.Files {
				if f.Indexseedhashhex < first.Indexseedhashhex {
					first = f
				}
			}
			display = first.Display
		}
		if entry.Selected != nil {
			for _, u := range entry.URLs {
				if entry.Selected.URL == u.URL {
					display = u.Display
				}
			}
			for _, f := range entry.Files {
				if entry.Selected.Indexseedhashhex == f.Indexseedhashhex {
					display = f.Display
				}
			}
		}
		entry.Name, entry.Description, entry.Icon = display.Name, display.Description, display.Icon
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Favorite && !out[j].Favorite })
	return out
}
func publicDirectoryEntries(entries []DirectoryEntry) []DirectoryEntry {
	for i := range entries {
		entries[i].PublicationID = ""
		entries[i].URLs = slices.Clone(entries[i].URLs)
		for j := range entries[i].URLs {
			entries[i].URLs[j].PublicationID = ""
		}
		entries[i].Files = slices.Clone(entries[i].Files)
		for j := range entries[i].Files {
			entries[i].Files[j].PublicationID = ""
		}
	}
	return entries
}

func (s *DirectoryStore) List() []DirectoryEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return publicDirectoryEntries(s.listLocked())
}

// FilesPublicationID 返回当前目录中指定 App 公钥与 Files seed 对应渠道的
// 证明摘要，供目录启动在 Go 后台核对实际部署。目录条目不存在、渠道已变更
// 或证明摘要为空均返回错误，调用方须拒绝目录启动。
func (s *DirectoryStore) FilesPublicationID(appPublicKeyHex, seed string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ApplicationKey(appPublicKeyHex)
	if key == "" {
		return "", fmt.Errorf("App public key required")
	}
	if !isSeed(seed) {
		return "", fmt.Errorf("invalid Files seed")
	}
	entry, ok := s.state.Apps[key]
	if !ok {
		return "", fmt.Errorf("目录条目不存在")
	}
	for _, channel := range entry.Files {
		if channel.Indexseedhashhex == seed {
			if channel.PublicationID == "" {
				return "", fmt.Errorf("目录证明摘要缺失")
			}
			return channel.PublicationID, nil
		}
	}
	return "", fmt.Errorf("目录 Files 渠道已变更")
}

// View 只读本地已归属数据；不发请求、不触发市场刷新。
func (s *DirectoryStore) View() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"entries": publicDirectoryEntries(s.listLocked()), "favorites": s.state.Favorites, "deletedKeys": append([]string{}, s.state.DeletedKeys...)}
	if s.loadError != nil {
		out["directoryError"] = s.loadError.Error()
	}
	return out
}

// Remove 是用户手动删除：移除目录条目、来源关联与收藏，并写入删除标记，
// 阻止来源插件的重放恢复。待验证项归 Market，不经本方法。
func (s *DirectoryStore) Remove(key string) error {
	if ApplicationKey(key) == "" {
		return fmt.Errorf("App public key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactLocked(func(f *directoryFile) error {
		delete(f.Apps, key)
		delete(f.Sources, key)
		delete(f.Favorites, key)
		delete(f.Selection, key)
		if !slices.Contains(f.DeletedKeys, key) {
			f.DeletedKeys = append(f.DeletedKeys, key)
		}
		return nil
	})
}

// RestoreApp 只清除删除标记：新的已验证连接可恢复该 App 的管理资格，
// 不写发布资料。
func (s *DirectoryStore) RestoreApp(key string) error {
	if ApplicationKey(key) == "" {
		return fmt.Errorf("App public key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactLocked(func(f *directoryFile) error {
		f.DeletedKeys = slices.DeleteFunc(f.DeletedKeys, func(k string) bool { return k == key })
		return nil
	})
}

// Favorite 只保存本机置顶标记，不改变来源与发布资料。
func (s *DirectoryStore) Favorite(key string, value bool) error {
	if ApplicationKey(key) == "" {
		return fmt.Errorf("App public key required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transactLocked(func(f *directoryFile) error {
		if value {
			f.Favorites[key] = true
		} else {
			delete(f.Favorites, key)
			if _, ok := f.Sources[key]; !ok && f.Apps[key].AppVersion != "" {
				// 无来源且无使用判定的条目随取消收藏移除；使用判定由调用方
				// hasUsage 在来源失效路径处理，收藏路径保持简单删除。
				delete(f.Apps, key)
			}
		}
		if app, ok := f.Apps[key]; ok {
			app.Favorite = value
			f.Apps[key] = app
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
	app, ok := s.state.Apps[key]
	if !ok {
		return fmt.Errorf("App not found")
	}
	if !directoryHasSelection(app, selection) {
		return fmt.Errorf("selected channel not found")
	}
	return s.transactLocked(func(f *directoryFile) error {
		// 用户明确选择的入口持久保存；来源更新不覆盖。
		f.Selection[key] = selection
		return nil
	})
}
