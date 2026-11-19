// Package market 拥有市场目录同步与手工来源管理。
//
// 市场配置与按来源同步记录归属 apps/market-state.json（版本 1，硬切换）：
// 内置市场固定存在，第三方市场按名称与完整目录 URL 登记，来源 ID 由本层
// 分配并保证唯一。手工来源与待验证项同文件保存。市场拉取在命令队列之外
// 执行；提交 Connect 前先落盘在途批次日志，记账成功后清除，重启或下一轮
// 同步前先收敛上一轮未记账结果，再按新清单计算差异。
package market

import (
	"encoding/json"
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// 手工项验证与同步状态；与 Web 侧三态模型一致。
const (
	ValidationPending  = "pending"
	ValidationFailed   = "failed"
	ValidationVerified = "verified"

	SyncUnsynced     = "unsynced"
	SyncSynced       = "synced"
	SyncFailed       = "sync-failed"
	SyncLowerVersion = "lower-version"
)

// ManualItem 是一条手工来源或待验证项；验证前只有用户输入和验证状态。
type ManualItem struct {
	PendingID           string                  `json:"pendingId"`
	Revision            int                     `json:"revision"`
	Kind                string                  `json:"kind"` // url | files
	URL                 string                  `json:"url,omitempty"`
	Indexseedhashhex    string                  `json:"indexseedhashhex,omitempty"`
	Validation          string                  `json:"validation"`
	ValidationError     string                  `json:"validationError,omitempty"`
	VerifiedPublication *connect.DirectoryEntry `json:"verifiedPublication,omitempty"`
	VerifiedAt          int64                   `json:"verifiedAt,omitempty"`
	SyncStatus          string                  `json:"syncStatus,omitempty"`
	SyncError           string                  `json:"syncError,omitempty"`
	CreatedAt           int64                   `json:"createdAt"`
	UpdatedAt           int64                   `json:"updatedAt"`
}

// RejectedItem 记录本轮被拒的变更，供设置页展示。
type RejectedItem struct {
	SourceID        string `json:"sourceId"`
	AppPublicKeyHex string `json:"appPublicKeyHex"`
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
}

// MarketConfig 是一个已登记市场的配置；ID 是来源关联键与持久身份，
// 展示名称变化不改变身份，替换目录 URL 按移除旧来源并添加新来源处理。
type MarketConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	DirectoryURL string `json:"directoryUrl"`
	BuiltIn      bool   `json:"builtIn"`
}

// syncRecord 是单个市场的同步记录。
type syncRecord struct {
	LastSuccessAt int64          `json:"lastSuccessAt"` // 0 = 尚未成功同步
	LastError     string         `json:"lastError"`
	LastRejected  []RejectedItem `json:"lastRejected"`
}

// journalRecord 是在途批次恢复日志：提交 Connect 前落盘，记账成功后清除。
// 重启或下一轮同步前重放同批提交（同源同内容幂等 unchanged，不改变提交
// 序号），按实际结果记账后清除；Connect 已写入而 Market 尚未记账的窗口
// 因此可恢复。
type journalRecord struct {
	MarketID  string                 `json:"marketId"`
	PendingID string                 `json:"pendingId,omitempty"`
	Revision  int                    `json:"revision,omitempty"`
	Changes   []connect.SourceChange `json:"changes"`
}

type marketFile struct {
	Version int                                          `json:"version"`
	Markets []MarketConfig                               `json:"markets"`
	Applied map[string]map[string]connect.DirectoryEntry `json:"applied"`
	Pending map[string]ManualItem                        `json:"pending"`
	Sync    map[string]syncRecord                        `json:"sync"`
	Journal *journalRecord                               `json:"journal,omitempty"`
}

// DefaultMarketID 是内置市场的持久来源 ID；dev 基址覆盖仅作用于内置市场。
const DefaultMarketID = "market-default"

// DefaultMarketDirectoryURL 是内置市场的生产完整目录 URL。
const DefaultMarketDirectoryURL = "https://market.bsv8.com/v1/apps"

func emptyMarketFile() marketFile {
	return marketFile{
		Version: 1,
		Markets: []MarketConfig{{ID: DefaultMarketID, Name: "内置市场", DirectoryURL: DefaultMarketDirectoryURL, BuiltIn: true}},
		Applied: map[string]map[string]connect.DirectoryEntry{},
		Pending: map[string]ManualItem{},
		Sync:    map[string]syncRecord{},
	}
}

type store struct {
	mu        sync.Mutex
	path      string
	state     marketFile
	loadError error
}

func newStore(dir string) *store {
	return &store{path: filepath.Join(dir, "market-state.json"), state: emptyMarketFile()}
}

// CanonicalDirectoryURL 校验并规范化完整目录 URL：HTTP(S)、无凭据、
// 无片段；第三方可使用任何路径或静态托管 JSON。
func CanonicalDirectoryURL(raw string) (string, error) {
	full, _, err := connect.CanonicalEntryURL(raw)
	if err != nil {
		return "", err
	}
	if len(full) > 8192 {
		return "", fmt.Errorf("directory URL too long")
	}
	return full, nil
}

func (s *store) load() error {
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
	var f marketFile
	err = json.Unmarshal(raw, &f)
	if err == nil && (f.Version != 1 || f.Markets == nil || f.Applied == nil || f.Pending == nil || f.Sync == nil) {
		err = fmt.Errorf("incompatible market state format")
	}
	if err == nil {
		err = validateMarkets(f.Markets)
	}
	if err == nil {
		for marketID, applied := range f.Applied {
			if marketByID(f.Markets, marketID) == nil {
				err = fmt.Errorf("applied list without market config")
				break
			}
			if err = connect.ValidateDirectorySnapshot(entriesOf(applied)); err != nil {
				break
			}
			for key, entry := range applied {
				if entry.AppPublicKeyHex != key {
					err = fmt.Errorf("incompatible applied market identity")
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil && f.Journal != nil {
		if f.Journal.MarketID != connect.SourceManual && marketByID(f.Markets, f.Journal.MarketID) == nil {
			err = fmt.Errorf("journal without market config")
		}
	}
	if err == nil {
		for id, item := range f.Pending {
			if item.PendingID != id || id == "" || (item.URL == "") == (item.Indexseedhashhex == "") {
				err = fmt.Errorf("invalid pending identity")
				break
			}
			if item.Kind != "url" && item.Kind != "files" ||
				item.Validation != ValidationPending && item.Validation != ValidationFailed && item.Validation != ValidationVerified {
				err = fmt.Errorf("invalid pending state")
				break
			}
			if item.VerifiedPublication != nil {
				if item.Validation != ValidationVerified || item.VerifiedPublication.AppPublicKeyHex == "" {
					err = fmt.Errorf("invalid verified publication")
					break
				}
				if err = connect.ValidateDirectorySnapshot([]connect.DirectoryEntry{*item.VerifiedPublication}); err != nil {
					break
				}
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

// validateMarkets 校验市场配置：恰一个内置市场、ID 唯一且格式受限、
// 名称与完整目录 URL 非空合法。
func validateMarkets(markets []MarketConfig) error {
	if len(markets) == 0 {
		return fmt.Errorf("market list empty")
	}
	builtin := 0
	seen := map[string]bool{}
	for _, market := range markets {
		if market.ID == "" || seen[market.ID] {
			return fmt.Errorf("market identity missing or duplicate")
		}
		seen[market.ID] = true
		if market.BuiltIn {
			builtin++
		}
		if market.Name == "" {
			return fmt.Errorf("market name required")
		}
		if _, err := CanonicalDirectoryURL(market.DirectoryURL); err != nil {
			return fmt.Errorf("market directory URL invalid")
		}
	}
	if builtin != 1 {
		return fmt.Errorf("exactly one built-in market is required")
	}
	return nil
}

func marketByID(markets []MarketConfig, id string) *MarketConfig {
	for i := range markets {
		if markets[i].ID == id {
			return &markets[i]
		}
	}
	return nil
}

func entriesOf(applied map[string]connect.DirectoryEntry) []connect.DirectoryEntry {
	out := make([]connect.DirectoryEntry, 0, len(applied))
	for _, entry := range applied {
		out = append(out, entry)
	}
	return out
}

// transactLocked 先写完整候选状态（tmp+rename），成功才替换内存。
func (s *store) transactLocked(change func(*marketFile) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadError != nil {
		return s.loadError
	}
	raw, _ := json.Marshal(s.state)
	var next marketFile
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err := change(&next); err != nil {
		return err
	}
	if err := validateMarkets(next.Markets); err != nil {
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

func (s *store) snapshot() marketFile {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := emptyMarketFile()
	raw, _ := json.Marshal(s.state)
	_ = json.Unmarshal(raw, &out)
	return out
}

func nowMillis() int64 { return time.Now().UnixMilli() }

// sortedItems 按创建时间稳定排序，供状态视图使用。
func sortedItems(items []ManualItem) []ManualItem {
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt < items[j].CreatedAt })
	return items
}

// displayOfVerified 取已验证发布的展示资料；无发布时返回空。
func displayOfVerified(item ManualItem) apppublication.AppDisplay {
	if item.VerifiedPublication == nil {
		return apppublication.AppDisplay{}
	}
	if len(item.VerifiedPublication.URLs) > 0 {
		return item.VerifiedPublication.URLs[0].Display
	}
	if len(item.VerifiedPublication.Files) > 0 {
		return item.VerifiedPublication.Files[0].Display
	}
	return apppublication.AppDisplay{}
}
