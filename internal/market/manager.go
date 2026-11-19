package market

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	"github.com/bsv8/keymaster.cc-cli/internal/connect"
	"log"
	"sync"
	"time"
)

// SeedVerification 是 Files 验证（部署安装）返回的已验证结果。
type SeedVerification struct {
	AppPublicKeyHex string
	AppVersion      string
	PublicationID   string
	Display         apppublication.AppDisplay
}

// Fetcher 拉取指定市场的完整目录响应；失败、截断、结构错误或验证失败整轮失败。
type Fetcher func(ctx context.Context, directoryURL string) ([]connect.DirectoryEntry, error)

// URLVerifier 获取真实入口并验签发布证明；返回完整发布（URL 渠道）。
type URLVerifier func(ctx context.Context, rawURL string) (connect.DirectoryEntry, error)

// SeedVerifier 按 indexseedhashhex 完成整包下载验证。
type SeedVerifier func(ctx context.Context, seed string) (SeedVerification, error)

// Deps 由可信装配注入；目录写入的唯一调用方是本 Manager。
type Deps struct {
	BuiltInDirectoryURL string // 本次运行的完整内置市场 URL
	Dir                 *connect.DirectoryStore
	HasUsage            func(key string) bool // 无法确认时必须返回 true（保留）
	FetchMarket         Fetcher
	VerifyURL           URLVerifier
	VerifySeed          SeedVerifier
	OnChange            func() // 目录变化后通知（publishConnect）
}

// inflightSync 让同一市场的并发刷新共用在途任务：后到者等待同一次结果。
type inflightSync struct {
	done   chan struct{}
	cancel context.CancelFunc
	err    error
}

// Manager 串行化所有状态变更：市场拉取在队列外按市场独立进行，提交、
// 记账与移除经 opMu 依次执行。后台任务绑定 daemon 生命周期 ctx。
type Manager struct {
	deps       Deps
	store      *store
	ctx        context.Context
	wg         sync.WaitGroup // 后台任务退出跟踪，关闭时等待归零
	opMu       sync.Mutex     // 串行化提交/记账/移除，杜绝孤儿来源
	inflightMu sync.Mutex
	inflight   map[string]*inflightSync // marketID → 在途同步
	firstOnce  sync.Once
}

func NewManager(ctx context.Context, dir string, deps Deps) (*Manager, error) {
	st := newStore(dir)
	if err := st.load(); err != nil {
		return nil, err
	}
	m := &Manager{deps: deps, store: st, ctx: ctx, inflight: map[string]*inflightSync{}}
	// 先收敛上一轮在途批次：Connect 已写入而未记账的提交按实际结果记账。
	m.opMu.Lock()
	err := m.settleLocked()
	m.opMu.Unlock()
	if err != nil {
		log.Printf("[market] 收敛在途批次失败，下次同步或来源写入将重试: %v", err)
	}
	return m, nil
}

// Wait 等待全部后台任务退出；daemon 关闭时在释放数据目录锁之前调用。
func (m *Manager) Wait() { m.wg.Wait() }

// Ready 报告存储是否可写；载入失败时拒绝一切业务操作。
func (m *Manager) Ready() error {
	if m.store.loadError != nil {
		return m.store.loadError
	}
	return nil
}

// firstSyncTimeout 限定单市场首次同步的总时长；目录拉取客户端另有自身超时。
const firstSyncTimeout = 60 * time.Second

// FirstSync 在钱包身份与存储就绪后对每个市场各触发一次同步；每轮后台
// 运行只执行一次。后台任务绑定 daemon 生命周期，关闭时取消并等待退出。
func (m *Manager) FirstSync() {
	m.firstOnce.Do(func() {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() { _ = recover() }()
			ctx, cancel := context.WithTimeout(m.ctx, firstSyncTimeout)
			defer cancel()
			_ = m.SyncAll(ctx)
			m.deps.OnChange()
		}()
	})
}

// SyncAll 对全部已登记市场各执行一轮同步；不同市场独立拉取，单个市场
// 挂起或失败不阻塞其他市场。
func (m *Manager) SyncAll(ctx context.Context) error {
	snapshot := m.store.snapshot()
	ids := make([]string, 0, len(snapshot.Markets))
	for _, market := range snapshot.Markets {
		ids = append(ids, market.ID)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(ids))
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errs[i] = fmt.Errorf("market sync panic")
				}
			}()
			errs[i] = m.SyncMarket(ctx, id)
		}(i, id)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// SyncMarket 执行单个市场的一轮同步：拉取在 opMu 之外，提交与记账进入
// opMu。同一市场的并发刷新共用在途任务；进行中时后到者等待同一结果。
func (m *Manager) SyncMarket(ctx context.Context, marketID string) error {
	if err := m.Ready(); err != nil {
		return err
	}
	// 在途共用：登记或等待既有任务。
	for {
		m.inflightMu.Lock()
		if running, ok := m.inflight[marketID]; ok {
			m.inflightMu.Unlock()
			select {
			case <-running.done:
				return running.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		snapshot := m.store.snapshot()
		config := marketByID(snapshot.Markets, marketID)
		if config == nil {
			m.inflightMu.Unlock()
			return fmt.Errorf("市场不存在: %s", marketID)
		}
		taskCtx, cancel := context.WithCancel(ctx)
		running := &inflightSync{done: make(chan struct{}), cancel: cancel}
		m.inflight[marketID] = running
		m.inflightMu.Unlock()
		return func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("market sync panic")
				}
				m.inflightMu.Lock()
				running.err = err
				cancel()
				close(running.done)
				delete(m.inflight, marketID)
				m.inflightMu.Unlock()
			}()
			// 市场拉取在命令队列之外：请求挂起时不阻塞手工命令与其他市场。
			entries, fetchErr := m.deps.FetchMarket(taskCtx, m.directoryURL(*config))
			m.opMu.Lock()
			defer m.opMu.Unlock()
			// 市场在拉取期间被移除：迟到结果直接丢弃，不写任何状态。
			if current := marketByID(m.store.state.Markets, marketID); current == nil {
				return nil
			}
			return m.applySyncLocked(marketID, entries, fetchErr)
		}()
	}
}

// applySyncLocked 在 opMu 内完成单市场差异计算、journal 保护的批次提交
// 与记账。先收敛上一轮未记账结果，再按该市场的新清单计算差异；目录写盘
// 失败整轮作废，不更新已应用清单与成功时间。
func (m *Manager) applySyncLocked(marketID string, entries []connect.DirectoryEntry, fetchErr error) error {
	if lifecycleErr := m.ctx.Err(); lifecycleErr != nil {
		return lifecycleErr
	}
	if err := m.settleLocked(); err != nil {
		return err
	}
	if lifecycleErr := m.ctx.Err(); lifecycleErr != nil {
		return lifecycleErr
	}
	fetchMessage := ""
	if fetchErr != nil {
		fetchMessage = fetchErr.Error()
	}
	applied := m.store.state.Applied[marketID]
	if applied == nil {
		applied = map[string]connect.DirectoryEntry{}
	}
	changes := []connect.SourceChange{}
	var fresh map[string]connect.DirectoryEntry
	if fetchErr == nil {
		fresh = map[string]connect.DirectoryEntry{}
		for _, entry := range entries {
			fresh[entry.AppPublicKeyHex] = entry
		}
		for _, entry := range entries {
			key := entry.AppPublicKeyHex
			if old, ok := applied[key]; !ok || !sameContent(old, entry) {
				changes = append(changes, connect.SourceChange{
					Kind: "submit", SourceID: marketID,
					AppPublicKeyHex: key, AppVersion: entry.AppVersion, Publication: entry,
				})
			}
		}
		for key, old := range applied {
			if _, ok := fresh[key]; !ok {
				changes = append(changes, connect.SourceChange{
					Kind: "invalidate", SourceID: marketID, AppPublicKeyHex: old.AppPublicKeyHex,
				})
			}
		}
	}
	results, rejected, batchErr := m.commitLocked(marketID, changes)
	if batchErr != nil {
		// 目录写盘失败：整轮作废。不更新已应用清单、不推进成功时间；
		// journal 保留，下一轮先收敛再重试。
		return m.store.transactLocked(func(f *marketFile) error {
			record := f.Sync[marketID]
			record.LastError = batchErr.Error()
			f.Sync[marketID] = record
			return nil
		})
	}
	return m.store.transactLocked(func(f *marketFile) error {
		accountBatch(f, marketID, changes, results, rejected)
		// 成功记账与清除本批日志同一次写入，避免中断后重放。
		if f.Journal != nil && f.Journal.MarketID == marketID {
			f.Journal = nil
		}
		if fetchErr == nil {
			record := f.Sync[marketID]
			record.LastSuccessAt = nowMillis()
			f.Sync[marketID] = record
		}
		f.Sync[marketID] = withSyncError(f.Sync[marketID], fetchMessage)
		return nil
	})
}

// commitLocked 执行一次受 journal 保护的批次提交（调用方持有 opMu）：
// 提交前把完整变更落盘，Connect 写盘成功并返回逐项结果后由调用方记账；
// 记账 transact 失败时 journal 保留，重启或下一轮经 settleLocked 收敛。
func (m *Manager) commitLocked(marketID string, changes []connect.SourceChange) ([]connect.SourceChangeResult, []RejectedItem, error) {
	if len(changes) > 0 {
		if err := m.store.transactLocked(func(f *marketFile) error {
			f.Journal = &journalRecord{MarketID: marketID, Changes: changes}
			return nil
		}); err != nil {
			return nil, nil, err
		}
	}
	results, batchErr := m.deps.Dir.ApplyBatch(changes, m.deps.HasUsage)
	if batchErr != nil {
		return nil, nil, batchErr
	}
	rejected := []RejectedItem{}
	for _, result := range results {
		if result.Status != "applied" && result.Status != "unchanged" {
			rejected = append(rejected, RejectedItem{
				SourceID: result.SourceID, AppPublicKeyHex: result.AppPublicKeyHex,
				Status: result.Status, Reason: result.Reason,
			})
		}
	}
	return results, rejected, nil
}

// accountBatch 按已持久化的逐项结果更新单个市场的已应用清单：submit 的
// applied/unchanged 记账，invalidate 的 applied/unchanged 撤销对应公钥；
// 其他状态记入被拒清单，不替换已应用记录。记账只处理本轮实际提交的
// 变更，不从清单外推导下架。只有写盘成功的结果才到达这里。
func accountBatch(f *marketFile, marketID string, changes []connect.SourceChange, results []connect.SourceChangeResult, rejected []RejectedItem) {
	if f.Applied[marketID] == nil {
		f.Applied[marketID] = map[string]connect.DirectoryEntry{}
	}
	for i, change := range changes {
		if i >= len(results) {
			break
		}
		result := results[i]
		switch {
		case change.Kind == "submit" && (result.Status == "applied" || result.Status == "unchanged"):
			f.Applied[marketID][change.AppPublicKeyHex] = change.Publication
		case change.Kind == "invalidate" && (result.Status == "applied" || result.Status == "unchanged"):
			delete(f.Applied[marketID], change.AppPublicKeyHex)
		}
	}
	record := f.Sync[marketID]
	record.LastRejected = rejected
	f.Sync[marketID] = record
}

func withSyncError(record syncRecord, message string) syncRecord {
	record.LastError = message
	return record
}

// settleLocked 收敛上一轮在途批次（调用方持有 opMu）：重放同批提交——
// 同源同内容幂等 unchanged，不改变提交序号——按实际结果记账后清除
// journal；记账失败保留 journal 供再次收敛。
func (m *Manager) settleLocked() error {
	snapshot := m.store.snapshot()
	journal := snapshot.Journal
	if journal == nil {
		return nil
	}
	if journal.MarketID == connect.SourceManual {
		item, ok := snapshot.Pending[journal.PendingID]
		if !ok || item.Revision != journal.Revision || item.Validation != ValidationVerified ||
			item.VerifiedPublication == nil || len(journal.Changes) != 1 {
			return fmt.Errorf("手工来源恢复记录与待同步发布不一致")
		}
		change := journal.Changes[0]
		publication := item.VerifiedPublication
		if change.Kind != "submit" || change.SourceID != connect.SourceManual ||
			change.AppPublicKeyHex != publication.AppPublicKeyHex || change.AppVersion != publication.AppVersion ||
			!sameContent(change.Publication, *publication) {
			return fmt.Errorf("手工来源恢复记录与待同步发布不一致")
		}
		results, err := m.deps.Dir.ApplyBatch(journal.Changes, m.deps.HasUsage)
		if err != nil {
			return err
		}
		if len(results) != 1 {
			return fmt.Errorf("Connect 未返回手工来源结果")
		}
		return m.store.transactLocked(func(f *marketFile) error {
			accountManual(f, journal.PendingID, results[0])
			f.Journal = nil
			return nil
		})
	}
	if marketByID(snapshot.Markets, journal.MarketID) == nil {
		// 配置已被移除：丢弃遗留 journal。
		return m.store.transactLocked(func(f *marketFile) error {
			f.Journal = nil
			return nil
		})
	}
	results, rejected, batchErr := m.commitLocked(journal.MarketID, journal.Changes)
	if batchErr != nil {
		return batchErr
	}
	return m.store.transactLocked(func(f *marketFile) error {
		if f.Journal == nil || f.Journal.MarketID != journal.MarketID {
			return nil
		}
		accountBatch(f, journal.MarketID, journal.Changes, results, rejected)
		f.Journal = nil
		return nil
	})
}

// directoryURL resolves the built-in address at runtime. A persisted dev address
// from older runs has no effect on the next daemon.
func (m *Manager) directoryURL(config MarketConfig) string {
	if !config.BuiltIn {
		return config.DirectoryURL
	}
	if m.deps.BuiltInDirectoryURL != "" {
		return m.deps.BuiltInDirectoryURL
	}
	return DefaultMarketDirectoryURL
}

// Configure 登记一个第三方市场：名称与完整目录 URL 必填，同一完整目录
// 地址只登记一次；新市场分配新 ID，登记后异步首拉一次。
func (m *Manager) Configure(name, directoryURL string) (string, error) {
	if err := m.Ready(); err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("市场名称必填")
	}
	full, err := CanonicalDirectoryURL(directoryURL)
	if err != nil {
		return "", fmt.Errorf("目录 URL 非法: %w", err)
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	for _, market := range m.store.state.Markets {
		if m.directoryURL(market) == full {
			return "", fmt.Errorf("该目录地址已登记为市场 %s", market.Name)
		}
	}
	id := "market_" + randomID()
	err = m.store.transactLocked(func(f *marketFile) error {
		for _, market := range f.Markets {
			if m.directoryURL(market) == full {
				return fmt.Errorf("该目录地址已登记")
			}
		}
		f.Markets = append(f.Markets, MarketConfig{ID: id, Name: name, DirectoryURL: full})
		f.Sync[id] = syncRecord{}
		return nil
	})
	if err != nil {
		return "", err
	}
	m.deps.OnChange()
	// 新市场首拉一次；后台任务绑定 daemon 生命周期。
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { _ = recover() }()
		ctx, cancel := context.WithTimeout(m.ctx, firstSyncTimeout)
		defer cancel()
		_ = m.SyncMarket(ctx, id)
		m.deps.OnChange()
	}()
	return id, nil
}

// Discard 移除一个第三方市场：先收敛未记账批次，再按最新已应用清单提交
// 该来源全部失效（journal 保护），成功后删除配置、已应用清单与同步记录；
// 在途请求取消，迟到结果按配置缺席丢弃。内置市场不可移除。Connect 不可用、
// 收敛失败或记账失败时返回失败，保留配置和待恢复记录。
func (m *Manager) Discard(marketID string) error {
	if err := m.Ready(); err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	config := marketByID(m.store.state.Markets, marketID)
	if config == nil {
		return fmt.Errorf("市场不存在: %s", marketID)
	}
	if config.BuiltIn {
		return fmt.Errorf("内置市场不可移除")
	}
	// 取消该市场在途拉取；提交段的配置检查丢弃迟到结果。
	m.inflightMu.Lock()
	if running, ok := m.inflight[marketID]; ok {
		running.cancel()
	}
	m.inflightMu.Unlock()
	// 先收敛上一轮待提交或待记账的结果，再按该来源的新清单计算差异，避免孤儿来源。
	if err := m.settleLocked(); err != nil {
		return fmt.Errorf("收敛未记账批次失败: %w", err)
	}
	if m.store.state.Journal != nil {
		return fmt.Errorf("Connect 不可用，无法收敛未记账批次")
	}
	if marketByID(m.store.state.Markets, marketID) == nil {
		return fmt.Errorf("市场不存在: %s", marketID)
	}
	applied := m.store.state.Applied[marketID]
	changes := make([]connect.SourceChange, 0, len(applied))
	for _, entry := range applied {
		changes = append(changes, connect.SourceChange{
			Kind: "invalidate", SourceID: marketID, AppPublicKeyHex: entry.AppPublicKeyHex,
		})
	}
	if _, _, batchErr := m.commitLocked(marketID, changes); batchErr != nil {
		return fmt.Errorf("来源失效失败: %w", batchErr)
	}
	return m.store.transactLocked(func(f *marketFile) error {
		delete(f.Applied, marketID)
		delete(f.Sync, marketID)
		if f.Journal != nil && f.Journal.MarketID == marketID {
			f.Journal = nil
		}
		f.Markets = slicesWithoutMarket(f.Markets, marketID)
		return nil
	})
}

func slicesWithoutMarket(markets []MarketConfig, id string) []MarketConfig {
	out := make([]MarketConfig, 0, len(markets))
	for _, market := range markets {
		if market.ID != id {
			out = append(out, market)
		}
	}
	return out
}

// Add 保存一条待验证项；验证异步进行，验签通过后直接提交 Connect。
// 验证不绑定调用方请求的上下文：ws 响应返回后请求取消，验证继续完成。
func (m *Manager) Add(url, seed string) (string, error) {
	if (url == "") == (seed == "") {
		return "", fmt.Errorf("exactly one URL or seed is required")
	}
	if err := m.Ready(); err != nil {
		return "", err
	}
	kind, full := "files", seed
	if url != "" {
		var err error
		full, _, err = connect.CanonicalEntryURL(url)
		if err != nil {
			return "", err
		}
		kind, seed = "url", ""
	}
	m.opMu.Lock()
	for _, old := range m.store.state.Pending {
		if old.Kind == kind && old.URL == full || old.Kind == kind && old.Indexseedhashhex == full {
			m.opMu.Unlock()
			return "", fmt.Errorf("该入口已在待验证列表中")
		}
	}
	at := nowMillis()
	item := ManualItem{
		PendingID: "pending_" + randomID(), Revision: 1, Kind: kind,
		URL:              map[bool]string{true: full, false: ""}[kind == "url"],
		Indexseedhashhex: map[bool]string{true: full, false: ""}[kind == "files"],
		Validation:       ValidationPending, SyncStatus: SyncUnsynced, CreatedAt: at, UpdatedAt: at,
	}
	err := m.store.transactLocked(func(f *marketFile) error {
		f.Pending[item.PendingID] = item
		return nil
	})
	m.opMu.Unlock()
	if err != nil {
		return "", err
	}
	m.deps.OnChange()
	// 验证在 opMu 之外执行（网络/下载）；结果落盘在 opMu 内按修订号核对。
	// 后台任务纳入退出跟踪：daemon 关闭时取消并等待。
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() { _ = recover() }()
		m.verifyItem(item.PendingID, 1)
	}()
	return item.PendingID, nil
}

// verifyTimeout 限定单次验证的总时长；URL 抓取与 Files 安装各自另有超时。
const verifyTimeout = 15 * time.Minute

// verifyItem 验证一条待验证项；验证执行中删除或修改输入时，迟到结果按修订号丢弃。
// 上下文脱离调用方请求（验证必须在请求返回后继续完成），但跟随 daemon 生命周期。
func (m *Manager) verifyItem(pendingID string, revision int) {
	ctx, cancel := context.WithTimeout(m.ctx, verifyTimeout)
	defer cancel()
	var verified *connect.DirectoryEntry
	var failure string
	snapshot := m.store.snapshot()
	item, ok := snapshot.Pending[pendingID]
	if !ok || item.Revision != revision {
		return
	}
	if item.Kind == "url" {
		entry, err := m.deps.VerifyURL(ctx, item.URL)
		if err != nil {
			failure = err.Error()
		} else {
			verified = &entry
		}
	} else {
		result, err := m.deps.VerifySeed(ctx, item.Indexseedhashhex)
		if err != nil {
			failure = err.Error()
		} else {
			entry := connect.DirectoryEntry{
				PublicationID: result.PublicationID, AppVersion: result.AppVersion,
				AppPublicKeyHex: result.AppPublicKeyHex, Name: result.Display.Name,
				Description: result.Display.Description, Icon: result.Display.Icon,
				URLs: []connect.DirectoryURL{}, Files: []connect.DirectoryFiles{
					{PublicationID: result.PublicationID, Display: result.Display, Indexseedhashhex: item.Indexseedhashhex},
				},
			}
			verified = &entry
		}
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	// daemon 关闭后迟到的验证结果直接丢弃，不再写状态或提交目录。
	if m.ctx.Err() != nil {
		return
	}
	if err := m.store.transactLocked(func(f *marketFile) error {
		current, ok := f.Pending[pendingID]
		if !ok || current.Revision != revision {
			return nil // 迟到结果丢弃
		}
		current.UpdatedAt = nowMillis()
		if failure != "" {
			current.Validation = ValidationFailed
			current.ValidationError = failure
			f.Pending[pendingID] = current
			return nil
		}
		current.Validation = ValidationVerified
		current.ValidationError = ""
		current.VerifiedPublication = verified
		current.VerifiedAt = nowMillis()
		current.SyncStatus = SyncUnsynced
		current.SyncError = ""
		// 同公钥旧记录不在此处删除：Connect 接受本次提交后才归并，
		// 低版本被拒时旧来源的管理记录原样保留。
		f.Pending[pendingID] = current
		return nil
	}); err != nil {
		return
	}
	m.deps.OnChange()
	if failure == "" {
		// 验签通过后提交进入命令队列：与移除依次执行，不留孤儿来源。
		_ = m.submitManualLocked(pendingID)
		m.deps.OnChange()
	}
}

// submitManualLocked 把已验证未同步项直接提交 Connect（调用方持有 opMu）；
// 不经市场拉取，市场无响应时不被阻塞。Connect 接受后才归并同公钥旧记录。
func (m *Manager) submitManualLocked(pendingID string) error {
	if err := m.settleLocked(); err != nil {
		return m.recordManualSyncError(pendingID, err)
	}
	item, ok := m.store.state.Pending[pendingID]
	if !ok || item.Validation != ValidationVerified || item.VerifiedPublication == nil ||
		(item.SyncStatus != SyncUnsynced && item.SyncStatus != SyncFailed) {
		return nil
	}
	publication := *item.VerifiedPublication
	if err := m.store.transactLocked(func(f *marketFile) error {
		f.Journal = &journalRecord{
			MarketID: connect.SourceManual, PendingID: pendingID, Revision: item.Revision,
			Changes: []connect.SourceChange{{
				Kind: "submit", SourceID: connect.SourceManual,
				AppPublicKeyHex: publication.AppPublicKeyHex, AppVersion: publication.AppVersion, Publication: publication,
			}},
		}
		return nil
	}); err != nil {
		return err
	}
	if err := m.settleLocked(); err != nil {
		return m.recordManualSyncError(pendingID, err)
	}
	return nil
}

func (m *Manager) recordManualSyncError(pendingID string, cause error) error {
	return m.store.transactLocked(func(f *marketFile) error {
		item, ok := f.Pending[pendingID]
		if ok && item.Validation == ValidationVerified &&
			(item.SyncStatus == SyncUnsynced || item.SyncStatus == SyncFailed) {
			item.SyncStatus = SyncFailed
			item.SyncError = cause.Error()
			item.UpdatedAt = nowMillis()
			f.Pending[pendingID] = item
		}
		return nil
	})
}

func accountManual(f *marketFile, pendingID string, result connect.SourceChangeResult) {
	item := f.Pending[pendingID]
	item.UpdatedAt = nowMillis()
	switch result.Status {
	case "applied", "unchanged":
		item.SyncStatus = SyncSynced
		item.SyncError = ""
		key := item.VerifiedPublication.AppPublicKeyHex
		for otherID, other := range f.Pending {
			if otherID != pendingID && other.Validation == ValidationVerified && other.VerifiedPublication != nil &&
				other.VerifiedPublication.AppPublicKeyHex == key {
				delete(f.Pending, otherID)
			}
		}
	case "lower-version-rejected":
		item.SyncStatus = SyncLowerVersion
		item.SyncError = "已验证，版本较低，未更新目录"
	default:
		item.SyncStatus = SyncFailed
		item.SyncError = result.Reason
		if item.SyncError == "" {
			item.SyncError = "同步失败"
		}
	}
	f.Pending[pendingID] = item
}

// Remove 移除手工来源或待验证项；已同步来源先向 Connect 提交失效，再删 Market 记录。
// 未同步（验证失败、待同步、版本较低）的记录在 Connect 侧没有来源，只改 Market 数据。
func (m *Manager) Remove(pendingID string) error {
	if err := m.Ready(); err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := m.settleLocked(); err != nil {
		return err
	}
	item, ok := m.store.state.Pending[pendingID]
	if !ok {
		return nil
	}
	if item.Validation == ValidationVerified && item.VerifiedPublication != nil && item.SyncStatus == SyncSynced {
		results, batchErr := m.deps.Dir.ApplyBatch([]connect.SourceChange{{
			Kind: "invalidate", SourceID: connect.SourceManual,
			AppPublicKeyHex: item.VerifiedPublication.AppPublicKeyHex,
		}}, m.deps.HasUsage)
		if batchErr != nil {
			// 目录写盘失败：不删 Market 记录，保留已同步状态供重试移除。
			return fmt.Errorf("来源失效失败: %w", batchErr)
		}
		var result connect.SourceChangeResult
		if len(results) > 0 {
			result = results[0]
		}
		if result.Status != "applied" && result.Status != "unchanged" {
			return fmt.Errorf("来源失效失败: %s", result.Reason)
		}
	}
	err := m.store.transactLocked(func(f *marketFile) error {
		delete(f.Pending, pendingID)
		return nil
	})
	if err == nil {
		m.deps.OnChange()
	}
	return err
}

// Retry 重试验证或同步：未验证项重走验证（异步，不阻塞请求）；已验证未同步项
// 直接提交 Connect，不经市场拉取。lower-version 不是失败，重试不改其状态。
func (m *Manager) Retry(pendingID string) error {
	if err := m.Ready(); err != nil {
		return err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	item, ok := m.store.state.Pending[pendingID]
	if !ok {
		return fmt.Errorf("待验证项不存在")
	}
	if item.Validation != ValidationVerified {
		item.Revision++
		if err := m.store.transactLocked(func(f *marketFile) error {
			f.Pending[pendingID] = item
			return nil
		}); err != nil {
			return err
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			defer func() { _ = recover() }()
			m.verifyItem(pendingID, item.Revision)
		}()
		return nil
	}
	if err := m.submitManualLocked(pendingID); err != nil {
		return err
	}
	m.deps.OnChange()
	return nil
}

// StateView 返回设置页所需状态：市场列表（含各自同步记录）、待验证项、
// 已验证手工来源与已应用市场条目总数。
func (m *Manager) StateView() map[string]any {
	snapshot := m.store.snapshot()
	pending := []map[string]any{}
	manual := []map[string]any{}
	for _, item := range sortedItems(itemsOf(snapshot.Pending)) {
		view := itemView(item)
		if item.Validation == ValidationVerified {
			manual = append(manual, view)
		} else {
			pending = append(pending, view)
		}
	}
	markets := []map[string]any{}
	appliedTotal := 0
	for _, config := range snapshot.Markets {
		record := snapshot.Sync[config.ID]
		state := map[string]any{
			"id": config.ID, "name": config.Name, "directoryUrl": m.directoryURL(config),
			"builtIn": config.BuiltIn, "lastSuccessAt": record.LastSuccessAt,
		}
		if record.LastError == "" {
			state["lastError"] = nil
		} else {
			state["lastError"] = record.LastError
		}
		if record.LastRejected == nil {
			state["lastRejected"] = []RejectedItem{}
		} else {
			state["lastRejected"] = record.LastRejected
		}
		markets = append(markets, state)
		appliedTotal += len(snapshot.Applied[config.ID])
	}
	return map[string]any{
		"markets": markets, "pending": pending, "manual": manual,
		"appliedMarketCount": appliedTotal,
	}
}

func itemView(item ManualItem) map[string]any {
	view := map[string]any{
		"pendingId": item.PendingID, "kind": item.Kind, "validation": item.Validation,
		"updatedAt": item.UpdatedAt,
	}
	if item.URL != "" {
		view["url"] = item.URL
	}
	if item.Indexseedhashhex != "" {
		view["indexseedhashhex"] = item.Indexseedhashhex
	}
	if item.ValidationError != "" {
		view["validationError"] = item.ValidationError
	}
	if item.SyncStatus != "" {
		view["syncStatus"] = item.SyncStatus
	}
	if item.SyncError != "" {
		view["syncError"] = item.SyncError
	}
	if item.VerifiedPublication != nil {
		view["appVersion"] = item.VerifiedPublication.AppVersion
		view["appPublicKeyHex"] = item.VerifiedPublication.AppPublicKeyHex
		if display := displayOfVerified(item); display.Name != "" {
			view["name"] = display.Name
		}
	}
	return view
}

func itemsOf(pending map[string]ManualItem) []ManualItem {
	out := make([]ManualItem, 0, len(pending))
	for _, item := range pending {
		out = append(out, item)
	}
	return out
}

// sameContent 比较整份发布内容（版本+展示+渠道），同版本不只比 appVersion。
func sameContent(a, b connect.DirectoryEntry) bool {
	return contentJSON(a) == contentJSON(b)
}

func contentJSON(entry connect.DirectoryEntry) string {
	norm := connect.DirectoryEntry{
		AppVersion: entry.AppVersion, AppPublicKeyHex: entry.AppPublicKeyHex,
		Name: entry.Name, Description: entry.Description, Icon: entry.Icon,
		URLs: entry.URLs, Files: entry.Files,
	}
	raw, err := json.Marshal(norm)
	if err != nil {
		return ""
	}
	return string(raw)
}
