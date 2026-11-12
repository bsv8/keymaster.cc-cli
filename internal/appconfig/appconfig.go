// Package appconfig manages info.json and settings.json: the unified
// persistent store for network identity, sale roots and ordinary settings.
//
// key.json holds the official pure KeyHold document. info.json holds network,
// wallet public key, network binding and sale root list. settings.json holds
// ordinary parameters (port, theme, language, prices, scheduling). Business
// data (ledgers, tasks, sessions, recovery evidence, indexes) is untouched.
//
// Concurrency: the daemon serializes updates through this store (in-process
// mutex) plus the data directory file lock (cross-process). Each file carries
// a format version and a monotonic revision; updates based on a stale
// revision are rejected so an old management page cannot overwrite newer
// settings. Missing fields get defaults, illegal values fail. Corrupt,
// unknown-version or half-written files are never mistaken for a fresh
// install. Failed saves never report success.
package appconfig

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
)

const (
	// InfoVersion is the info.json format version.
	InfoVersion = 1
	// SettingsVersion is the settings.json format version.
	SettingsVersion = 1
	// DefaultPort is the daemon HTTP port default.
	DefaultPort = 2100
)

var (
	networkPattern = regexp.MustCompile(`^(main|test)$`)
	hex64Pattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pubkeyPattern  = regexp.MustCompile(`^(02|03)[0-9a-f]{64}$`)
	satsPattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	rootIDPattern  = regexp.MustCompile(`^r_[0-9a-f]{16}$`)
)

// Root is one sale root with a stable random ID.
type Root struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// Info is the info.json document. Roots and the default download root are
// mutable; network identity fields are not.
type Info struct {
	Version                 int    `json:"version"`
	Revision                int64  `json:"revision"`
	BsvNetwork              string `json:"bsvNetwork"`
	PublicKeyHex            string `json:"publicKeyHex"`
	NetworkAuthenticationHex string `json:"networkAuthenticationHex"`
	Roots                   []Root `json:"roots"`
	DefaultDownloadRootID   string `json:"defaultDownloadRootID,omitempty"`
}

// Scheduler holds buyer scheduling settings.
type Scheduler struct {
	MaxBlockPriceSatoshis string `json:"maxBlockPriceSatoshis"`
	PoolBlocks            int    `json:"poolBlocks"`
	TotalSeats            int    `json:"totalSeats"`
	FloatingSeats         int    `json:"floatingSeats"`
	RequestTimeoutMs      int    `json:"requestTimeoutMs"`
}

// Supplier is one SatSubscription gateway entry.
type Supplier struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	PublicKeyHex string   `json:"publicKeyHex"`
	Multiaddrs   []string `json:"multiaddrs"`
	Enabled      bool     `json:"enabled"`
}

// Services holds external service directories.
type Services struct {
	SatSuppliers        []Supplier             `json:"satSuppliers"`
	ArbitrationServices []arbitration.Endpoint `json:"arbitrationServices,omitempty"`
}

// DefaultCollectorEndpoint is the official collector submission URL.
// The official address is still TBD: confirm and replace before production.
const DefaultCollectorEndpoint = "https://collector.bsv8.com"

// CollectorEntry is one BitFSCollector submission target. Every submission
// is sent to every enabled collector.
type CollectorEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Enabled  bool   `json:"enabled"`
}

// Collector holds collector submission settings: multiple targets plus
// task interval parameters. Submission state itself is persisted separately
// (per collector) and never inside settings.
type Collector struct {
	Collectors []CollectorEntry `json:"collectors"`
	// EarlyTriggerCount queues at or above this size trigger an early submit.
	EarlyTriggerCount int `json:"earlyTriggerCount"`
	// SubmitIntervalMs is the periodic submit interval.
	SubmitIntervalMs int64 `json:"submitIntervalMs"`
	// RetryBaseMs/RetryCapMs bound the retry backoff.
	RetryBaseMs int64 `json:"retryBaseMs"`
	RetryCapMs  int64 `json:"retryCapMs"`
}

// ServerDefaults holds seller defaults. Per-file prices live in the pricing
// table, never as directory inheritance.
type ServerDefaults struct {
	DefaultSeedPriceSatoshis      string   `json:"defaultSeedPriceSatoshis"`
	DefaultFullBlockPriceSatoshis string   `json:"defaultFullBlockPriceSatoshis"`
	ArbiterPublicKeys             []string `json:"arbiterPublicKeys,omitempty"`
	Collector                     Collector `json:"collector"`
}

// Settings is the settings.json document. Port is resettable; the network
// identity lives in info.json.
type Settings struct {
	Version                int            `json:"version"`
	Revision               int64          `json:"revision"`
	Port                   int            `json:"port"`
	Theme                  string         `json:"theme"`
	Language               string         `json:"language"`
	WOCAPIKey              string         `json:"wocApiKey"`
	FeeRateSatoshisPerKb   int64          `json:"feeRateSatoshisPerKb"`
	Scheduler              Scheduler      `json:"scheduler"`
	Services               Services       `json:"services"`
	Server                 ServerDefaults `json:"server"`
}

// Default supplier identity (bsv8 gateway); address differs per network.
const (
	DefaultSupplierID           = "bsv8"
	DefaultSupplierName         = "bsv8"
	DefaultSupplierPublicKeyHex = "031e6690e2c12ef4f7b96204d6c96652625f7f300606f01f17f7b089c076425918"
	DefaultSupplierMainnet      = "/dns/us-gateway.bsv8.com/tcp/443/tls/ws/p2p/16Uiu2HAmEhfHraPUP1YrYQFy6M8TTbtCZq6A7BGcXi77ZwCS5jxj"
	DefaultSupplierTestnet      = "/dns/ustest-gateway.bsv8.com/tcp/443/tls/ws/p2p/16Uiu2HAmEhfHraPUP1YrYQFy6M8TTbtCZq6A7BGcXi77ZwCS5jxj"
)

var defaultPrices = map[string]string{"main": "10000", "test": "100"}

// DefaultPrices returns the network default price.
func DefaultPrices(network string) string {
	if v, ok := defaultPrices[network]; ok {
		return v
	}
	return "100"
}

func defaultSupplierAddress(network string) string {
	if network == "main" {
		return DefaultSupplierMainnet
	}
	return DefaultSupplierTestnet
}

// NewRootID generates a stable random root ID.
func NewRootID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("appconfig: 生成根 ID 失败: %w", err)
	}
	return "r_" + hex.EncodeToString(b[:]), nil
}

// Store manages one data directory's info.json + settings.json.
type Store struct {
	mu           sync.Mutex
	dir          string
	infoPath     string
	settingsPath string
	info         *Info
	settings     *Settings
}

// New opens the store for dir (paths only; use Load/Init to populate).
func New(dir string) *Store {
	return &Store{
		dir:          dir,
		infoPath:     filepath.Join(dir, "info.json"),
		settingsPath: filepath.Join(dir, "settings.json"),
	}
}

// Dir returns the data directory.
func (s *Store) Dir() string { return s.dir }

// InfoPath returns the info.json path (for diagnostics).
func (s *Store) InfoPath() string { return s.infoPath }

// SettingsPath returns the settings.json path (for diagnostics).
func (s *Store) SettingsPath() string { return s.settingsPath }

// writeFileAtomic writes data atomically with 0600.
func writeFileAtomic(path string, data []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(directory, ".tmp-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("替换文件失败: %w", err)
	}
	return nil
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

func validTheme(v string) bool {
	return v == "system" || v == "light" || v == "dark"
}

func validLanguage(v string) bool {
	// system follows the OS; otherwise BCP-47-ish short codes accepted.
	if v == "system" {
		return true
	}
	if len(v) < 2 || len(v) > 12 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validateInfo(in *Info) error {
	if in.Version != InfoVersion {
		return fmt.Errorf("info.json 版本不受支持: %d", in.Version)
	}
	if in.Revision < 0 {
		return errors.New("info.json revision 非法")
	}
	if !networkPattern.MatchString(in.BsvNetwork) {
		return fmt.Errorf("info.json 网络无效: %q", in.BsvNetwork)
	}
	if !pubkeyPattern.MatchString(in.PublicKeyHex) {
		return errors.New("info.json 公钥无效")
	}
	if !hex64Pattern.MatchString(in.NetworkAuthenticationHex) {
		return errors.New("info.json 网络绑定无效")
	}
	seenPath := map[string]bool{}
	seenID := map[string]bool{}
	for _, r := range in.Roots {
		if !rootIDPattern.MatchString(r.ID) {
			return fmt.Errorf("售卖根 ID 无效: %q", r.ID)
		}
		if seenID[r.ID] {
			return fmt.Errorf("售卖根 ID 重复: %q", r.ID)
		}
		seenID[r.ID] = true
		if strings.TrimSpace(r.Path) == "" || !filepath.IsAbs(r.Path) {
			return fmt.Errorf("售卖根路径必须是绝对路径: %q", r.Path)
		}
		cleaned := filepath.Clean(r.Path)
		if seenPath[cleaned] {
			return fmt.Errorf("售卖根路径重复: %q", r.Path)
		}
		seenPath[cleaned] = true
	}
	if in.DefaultDownloadRootID != "" && !seenID[in.DefaultDownloadRootID] {
		return errors.New("默认下载根不存在")
	}
	return nil
}

func validateSettings(st *Settings) error {
	if st.Version != SettingsVersion {
		return fmt.Errorf("settings.json 版本不受支持: %d", st.Version)
	}
	if st.Revision < 0 {
		return errors.New("settings.json revision 非法")
	}
	if !validPort(st.Port) {
		return errors.New("端口无效")
	}
	if !validTheme(st.Theme) {
		return fmt.Errorf("主题无效: %q", st.Theme)
	}
	if !validLanguage(st.Language) {
		return fmt.Errorf("语言无效: %q", st.Language)
	}
	if st.FeeRateSatoshisPerKb < 1 || st.FeeRateSatoshisPerKb > 1_000_000 {
		return errors.New("手续费率无效")
	}
	if !satsPattern.MatchString(st.Scheduler.MaxBlockPriceSatoshis) {
		return errors.New("调度器缺省块价无效")
	}
	if st.Scheduler.PoolBlocks < 1 || st.Scheduler.TotalSeats < 2 || st.Scheduler.FloatingSeats < 1 ||
		st.Scheduler.FloatingSeats >= st.Scheduler.TotalSeats || st.Scheduler.RequestTimeoutMs < 1 {
		return errors.New("调度器设置无效")
	}
	for _, price := range []string{st.Server.DefaultSeedPriceSatoshis, st.Server.DefaultFullBlockPriceSatoshis} {
		if !satsPattern.MatchString(price) {
			return errors.New("server 缺省售价无效")
		}
	}
	if err := validateCollector(&st.Server.Collector); err != nil {
		return err
	}
	for _, supplier := range st.Services.SatSuppliers {
		if supplier.ID == "" || !regexp.MustCompile(`^(02|03)[0-9a-f]{64}$`).MatchString(supplier.PublicKeyHex) || len(supplier.Multiaddrs) == 0 {
			return fmt.Errorf("供应商配置无效: %s", supplier.ID)
		}
	}
	for _, endpoint := range st.Services.ArbitrationServices {
		if _, err := endpoint.AddressInfo(); err != nil {
			return err
		}
	}
	if strings.Contains(st.WOCAPIKey, "\n") {
		return errors.New("WOC API Key 无效")
	}
	return nil
}

// validateCollector checks multi-target collector settings and intervals.
func validateCollector(c *Collector) error {
	seen := map[string]bool{}
	idPattern := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	for _, e := range c.Collectors {
		if !idPattern.MatchString(e.ID) {
			return fmt.Errorf("采集器 ID 非法: %q（仅允许字母数字 _ -，1~64 字符）", e.ID)
		}
		if seen[e.ID] {
			return fmt.Errorf("采集器 ID 重复: %q", e.ID)
		}
		seen[e.ID] = true
		if e.Enabled && strings.TrimSpace(e.Endpoint) == "" {
			return fmt.Errorf("启用的采集器缺少 endpoint: %q", e.ID)
		}
	}
	if c.EarlyTriggerCount < 1 || c.EarlyTriggerCount > 100000 {
		return errors.New("采集 earlyTriggerCount 非法")
	}
	if c.SubmitIntervalMs < 60000 || c.SubmitIntervalMs > 86400000 {
		return errors.New("采集 submitIntervalMs 非法（1 分钟~24 小时）")
	}
	if c.RetryBaseMs < 1000 || c.RetryBaseMs > 3600000 {
		return errors.New("采集 retryBaseMs 非法")
	}
	if c.RetryCapMs < c.RetryBaseMs || c.RetryCapMs > 86400000 {
		return errors.New("采集 retryCapMs 非法")
	}
	return nil
}

// defaultsCollector builds the default collector settings: one official
// target (URL TBD, disabled until confirmed) plus production intervals.
func defaultsCollector() Collector {
	return Collector{
		Collectors: []CollectorEntry{{
			ID: "official", Name: "官方",
			Endpoint: DefaultCollectorEndpoint, Enabled: false,
		}},
		EarlyTriggerCount: 1001,
		SubmitIntervalMs:  3600000,
		RetryBaseMs:       30000,
		RetryCapMs:        3600000,
	}
}

// NewCollectorID generates a stable collector entry ID.
func NewCollectorID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("appconfig: 生成采集器 ID 失败: %w", err)
	}
	return "c_" + hex.EncodeToString(b[:]), nil
}

// defaultsSettings builds fresh settings for network with revision 0.
func defaultsSettings(network string) *Settings {
	price := DefaultPrices(network)
	return &Settings{
		Version:              SettingsVersion,
		Revision:             0,
		Port:                 DefaultPort,
		Theme:                "system",
		Language:             "system",
		WOCAPIKey:            "",
		FeeRateSatoshisPerKb: 1000,
		Scheduler: Scheduler{
			MaxBlockPriceSatoshis: "0",
			PoolBlocks:            4,
			TotalSeats:            8,
			FloatingSeats:         2,
			RequestTimeoutMs:      5000,
		},
		Services: Services{
			SatSuppliers: []Supplier{{
				ID:           DefaultSupplierID,
				Name:         DefaultSupplierName,
				PublicKeyHex: DefaultSupplierPublicKeyHex,
				Multiaddrs:   []string{defaultSupplierAddress(network)},
				Enabled:      true,
			}},
		},
		Server: ServerDefaults{
			DefaultSeedPriceSatoshis:      price,
			DefaultFullBlockPriceSatoshis: price,
			Collector:                     defaultsCollector(),
		},
	}
}

// Init creates info.json + settings.json for a fresh wallet identity. It
// refuses to overwrite existing files so a half-failed init never looks
// complete.
func (s *Store) Init(network, publicKeyHex, networkAuthHex string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !networkPattern.MatchString(network) {
		return errors.New("网络必须是 main 或 test")
	}
	if !pubkeyPattern.MatchString(publicKeyHex) {
		return errors.New("公钥无效")
	}
	if !hex64Pattern.MatchString(networkAuthHex) {
		return errors.New("网络绑定无效")
	}
	for _, p := range []string{s.infoPath, s.settingsPath} {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("文件已存在，拒绝覆盖: %s", p)
		}
	}
	info := &Info{
		Version: InfoVersion, Revision: 0,
		BsvNetwork: network, PublicKeyHex: publicKeyHex,
		NetworkAuthenticationHex: networkAuthHex,
		Roots:                    []Root{},
	}
	if err := validateInfo(info); err != nil {
		return err
	}
	settings := defaultsSettings(network)
	if err := validateSettings(settings); err != nil {
		return err
	}
	infoRaw, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	settingsRaw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	// Recoverable two-file commit: write info first with a pending marker,
	// then settings, then clear the marker. A crash leaves the marker so the
	// next startup reports damage instead of a half-complete wallet.
	marker := filepath.Join(s.dir, ".init-pending")
	if err := writeFileAtomic(marker, []byte("init\n")); err != nil {
		return err
	}
	if err := writeFileAtomic(s.infoPath, append(infoRaw, '\n')); err != nil {
		_ = os.Remove(marker)
		return err
	}
	if err := writeFileAtomic(s.settingsPath, append(settingsRaw, '\n')); err != nil {
		_ = os.Remove(s.infoPath)
		_ = os.Remove(marker)
		return err
	}
	_ = os.Remove(marker)
	s.info = info
	s.settings = settings
	return nil
}

// Load reads and validates both files. Missing, corrupt, unknown-version or
// half-initialized state is an error, never treated as a fresh install.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Join(s.dir, ".init-pending")); err == nil {
		return errors.New("初始化未完成（存在 .init-pending），请重新执行 init 或从备份恢复")
	}
	infoRaw, err := os.ReadFile(s.infoPath)
	if err != nil {
		return fmt.Errorf("读取 info.json 失败: %w", err)
	}
	var info Info
	if err := json.Unmarshal(infoRaw, &info); err != nil {
		return fmt.Errorf("info.json 不是有效 JSON: %w", err)
	}
	// Unknown fields are rejected implicitly by strict re-encode check? Keep
	// permissive for forward-compatible reads but validate known fields.
	if err := validateInfo(&info); err != nil {
		return err
	}
	settingsRaw, err := os.ReadFile(s.settingsPath)
	if err != nil {
		return fmt.Errorf("读取 settings.json 失败: %w", err)
	}
	settingsRaw = migrateLegacyCollectorShape(settingsRaw)
	var settings Settings
	dec := json.NewDecoder(bytes.NewReader(settingsRaw))
	dec.DisallowUnknownFields()
	// DisallowUnknownFields would reject future fields; instead use plain
	// unmarshal and validate known fields so upgrades add fields safely.
	if err := json.Unmarshal(settingsRaw, &settings); err != nil {
		_ = dec
		return fmt.Errorf("settings.json 不是有效 JSON: %w", err)
	}
	applySettingsDefaults(&settings, info.BsvNetwork)
	if err := validateSettings(&settings); err != nil {
		return err
	}
	s.info = &info
	s.settings = &settings
	return nil
}

// applySettingsDefaults fills missing ordinary fields. It never changes the
// network identity or revision semantics: empty strings that are legitimate
// user values (e.g. WOCAPIKey) stay empty.
func applySettingsDefaults(st *Settings, network string) {
	if st.Port == 0 {
		st.Port = DefaultPort
	}
	if st.Theme == "" {
		st.Theme = "system"
	}
	if st.Language == "" {
		st.Language = "system"
	}
	if st.FeeRateSatoshisPerKb == 0 {
		st.FeeRateSatoshisPerKb = 1000
	}
	if st.Scheduler.PoolBlocks == 0 {
		st.Scheduler.PoolBlocks = 4
	}
	if st.Scheduler.TotalSeats == 0 {
		st.Scheduler.TotalSeats = 8
	}
	if st.Scheduler.FloatingSeats == 0 {
		st.Scheduler.FloatingSeats = 2
	}
	if st.Scheduler.RequestTimeoutMs == 0 {
		st.Scheduler.RequestTimeoutMs = 5000
	}
	if st.Scheduler.MaxBlockPriceSatoshis == "" {
		st.Scheduler.MaxBlockPriceSatoshis = "0"
	}
	if st.Server.DefaultSeedPriceSatoshis == "" {
		st.Server.DefaultSeedPriceSatoshis = DefaultPrices(network)
	}
	if st.Server.DefaultFullBlockPriceSatoshis == "" {
		st.Server.DefaultFullBlockPriceSatoshis = DefaultPrices(network)
	}
	if len(st.Services.SatSuppliers) == 0 {
		st.Services.SatSuppliers = []Supplier{{
			ID: DefaultSupplierID, Name: DefaultSupplierName,
			PublicKeyHex: DefaultSupplierPublicKeyHex,
			Multiaddrs:   []string{defaultSupplierAddress(network)},
			Enabled:      true,
		}}
	}
	migrateCollectorDefaults(st)
}

// migrateCollectorDefaults handles old single-endpoint collector files and
// fills multi-collector interval defaults. Old shape
// server.collector={enabled,endpoint} becomes one entry; missing intervals
// get production defaults. New files are untouched.
func migrateCollectorDefaults(st *Settings) {
	c := &st.Server.Collector
	if len(c.Collectors) == 0 && c.EarlyTriggerCount == 0 && c.SubmitIntervalMs == 0 && c.RetryBaseMs == 0 && c.RetryCapMs == 0 {
		// No collector section at all: fresh defaults (official, disabled).
		*c = defaultsCollector()
		return
	}
	if c.EarlyTriggerCount == 0 {
		c.EarlyTriggerCount = 1001
	}
	if c.SubmitIntervalMs == 0 {
		c.SubmitIntervalMs = 3600000
	}
	if c.RetryBaseMs == 0 {
		c.RetryBaseMs = 30000
	}
	if c.RetryCapMs == 0 {
		c.RetryCapMs = 3600000
	}
}

// migrateLegacyCollectorShape converts the pre-multi shape
// server.collector={enabled,endpoint} into Collectors entries. It runs on the
// raw JSON before typed unmarshal loses the old fields.
func migrateLegacyCollectorShape(raw []byte) []byte {
	var shell map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shell); err != nil {
		return raw
	}
	serverRaw, ok := shell["server"]
	if !ok {
		return raw
	}
	var server map[string]json.RawMessage
	if err := json.Unmarshal(serverRaw, &server); err != nil {
		return raw
	}
	collRaw, ok := server["collector"]
	if !ok {
		return raw
	}
	var coll map[string]json.RawMessage
	if err := json.Unmarshal(collRaw, &coll); err != nil {
		return raw
	}
	if _, has := coll["collectors"]; has {
		return raw
	}
	enabled := false
	endpoint := ""
	if v, ok := coll["enabled"]; ok {
		_ = json.Unmarshal(v, &enabled)
	}
	if v, ok := coll["endpoint"]; ok {
		_ = json.Unmarshal(v, &endpoint)
	}
	if !enabled && strings.TrimSpace(endpoint) == "" {
		return raw
	}
	entry := CollectorEntry{ID: "legacy", Name: "旧配置", Endpoint: endpoint, Enabled: enabled}
	collectors, _ := json.Marshal([]CollectorEntry{entry})
	dc := defaultsCollector()
	early, _ := json.Marshal(dc.EarlyTriggerCount)
	interval, _ := json.Marshal(dc.SubmitIntervalMs)
	base, _ := json.Marshal(dc.RetryBaseMs)
	capMs, _ := json.Marshal(dc.RetryCapMs)
	coll["collectors"] = collectors
	coll["earlyTriggerCount"] = early
	coll["submitIntervalMs"] = interval
	coll["retryBaseMs"] = base
	coll["retryCapMs"] = capMs
	delete(coll, "enabled")
	delete(coll, "endpoint")
	server["collector"], _ = json.Marshal(coll)
	shell["server"], _ = json.Marshal(server)
	out, err := json.Marshal(shell)
	if err != nil {
		return raw
	}
	return out
}

// Snapshot returns deep copies of the current state. Call Load or Init first.
func (s *Store) Snapshot() (Info, Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil || s.settings == nil {
		return Info{}, Settings{}, errors.New("appconfig: 尚未加载")
	}
	infoRaw, _ := json.Marshal(s.info)
	var info Info
	_ = json.Unmarshal(infoRaw, &info)
	settingsRaw, _ := json.Marshal(s.settings)
	var settings Settings
	_ = json.Unmarshal(settingsRaw, &settings)
	return info, settings, nil
}

// saveLocked persists both files atomically; caller holds s.mu and has
// already bumped revisions.
func (s *Store) saveLocked() error {
	infoRaw, err := json.MarshalIndent(s.info, "", "  ")
	if err != nil {
		return err
	}
	settingsRaw, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.infoPath, append(infoRaw, '\n')); err != nil {
		return err
	}
	if err := writeFileAtomic(s.settingsPath, append(settingsRaw, '\n')); err != nil {
		return err
	}
	return nil
}

// UpdateInfo applies fn to a copy of info, validates, bumps revision and
// persists. expectedRevision rejects stale writers.
func (s *Store) UpdateInfo(expectedRevision int64, fn func(*Info) error) (Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil || s.settings == nil {
		return Info{}, errors.New("appconfig: 尚未加载")
	}
	if expectedRevision != s.info.Revision {
		return Info{}, fmt.Errorf("info.json 已被更新（期望 revision %d，当前 %d），请刷新后重试", expectedRevision, s.info.Revision)
	}
	raw, _ := json.Marshal(s.info)
	var next Info
	_ = json.Unmarshal(raw, &next)
	// Network identity is not ordinarily mutable: guard it here so no caller
	// can switch networks by editing settings.
	origNetwork, origPub, origAuth := next.BsvNetwork, next.PublicKeyHex, next.NetworkAuthenticationHex
	if err := fn(&next); err != nil {
		return Info{}, err
	}
	if next.BsvNetwork != origNetwork || next.PublicKeyHex != origPub || next.NetworkAuthenticationHex != origAuth {
		return Info{}, errors.New("网络身份禁止普通修改")
	}
	next.Version = InfoVersion
	next.Revision++
	if err := validateInfo(&next); err != nil {
		return Info{}, err
	}
	prev := s.info
	s.info = &next
	if err := func() error {
		raw, err := json.MarshalIndent(s.info, "", "  ")
		if err != nil {
			return err
		}
		return writeFileAtomic(s.infoPath, append(raw, '\n'))
	}(); err != nil {
		s.info = prev
		return Info{}, err
	}
	out := *s.info
	return out, nil
}

// UpdateSettings applies fn to a copy of settings, validates, bumps revision
// and persists. It never fills defaults: the copy starts from the current
// valid state, so an explicitly provided zero value stays invalid instead of
// being masked by a default. Defaults are only filled on Load for old files.
func (s *Store) UpdateSettings(expectedRevision int64, fn func(*Settings) error) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.info == nil || s.settings == nil {
		return Settings{}, errors.New("appconfig: 尚未加载")
	}
	if expectedRevision != s.settings.Revision {
		return Settings{}, fmt.Errorf("settings.json 已被更新（期望 revision %d，当前 %d），请刷新后重试", expectedRevision, s.settings.Revision)
	}
	raw, _ := json.Marshal(s.settings)
	var next Settings
	_ = json.Unmarshal(raw, &next)
	if err := fn(&next); err != nil {
		return Settings{}, err
	}
	next.Version = SettingsVersion
	next.Revision++
	if err := validateSettings(&next); err != nil {
		return Settings{}, err
	}
	prev := s.settings
	s.settings = &next
	if err := func() error {
		raw, err := json.MarshalIndent(s.settings, "", "  ")
		if err != nil {
			return err
		}
		return writeFileAtomic(s.settingsPath, append(raw, '\n'))
	}(); err != nil {
		s.settings = prev
		return Settings{}, err
	}
	out := *s.settings
	return out, nil
}

// ResetSettings restores ordinary parameters to defaults (port included) and
// bumps revision. Identity and business data are untouched.
func (s *Store) ResetSettings(expectedRevision int64) (Settings, error) {
	return s.UpdateSettings(expectedRevision, func(next *Settings) error {
		fresh := defaultsSettings(s.info.BsvNetwork)
		// Preserve nothing except the fact that a reset happened: all
		// ordinary parameters return to defaults.
		*next = *fresh
		next.Revision = s.settings.Revision
		return nil
	})
}

// SetDefaultsPrices updates server default prices through the unified path.
func (s *Store) SetDefaultsPrices(expectedRevision int64, seedPrice, blockPrice string) (Settings, error) {
	return s.UpdateSettings(expectedRevision, func(next *Settings) error {
		next.Server.DefaultSeedPriceSatoshis = seedPrice
		next.Server.DefaultFullBlockPriceSatoshis = blockPrice
		return nil
	})
}

// SetCollectors replaces the collector target list through the unified path.
func (s *Store) SetCollectors(expectedRevision int64, entries []CollectorEntry) (Settings, error) {
	return s.UpdateSettings(expectedRevision, func(next *Settings) error {
		next.Server.Collector.Collectors = append([]CollectorEntry(nil), entries...)
		return nil
	})
}

// SetCollectorIntervals updates collector task interval parameters.
func (s *Store) SetCollectorIntervals(expectedRevision int64, early int, intervalMs, retryBaseMs, retryCapMs int64) (Settings, error) {
	return s.UpdateSettings(expectedRevision, func(next *Settings) error {
		next.Server.Collector.EarlyTriggerCount = early
		next.Server.Collector.SubmitIntervalMs = intervalMs
		next.Server.Collector.RetryBaseMs = retryBaseMs
		next.Server.Collector.RetryCapMs = retryCapMs
		return nil
	})
}
