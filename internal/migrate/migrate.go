// Package migrate moves legacy config.json + network-keyhold key.json to the
// unified layout: pure KeyHold key.json + info.json + settings.json.
//
// The old key.json is a custom envelope (keymaster.network-keyhold v1) that
// wraps an official KeyHold document plus an HMAC network binding. Migration
// verifies password, public key and HMAC first, then copies the inner KeyHold
// ciphertext verbatim (no re-encryption) to key.json and moves the binding to
// info.json.
//
// The whole swap is a recoverable transaction guarded by a pending marker:
// the new wallet is staged (key.json.staged) while the old files stay live,
// and the atomic rename happens last. A crash at any point either resumes
// (with the password prompted again by init) or rolls back to the original
// files; a half-swapped directory is never mistaken for a finished wallet.
package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	keyhold "github.com/bsv8/KeyHold/go/v2"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/config"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
)

const (
	markerName  = ".migrate-pending"
	stagedName  = "key.json.staged"
	markerStepParsed = "parsed"
)

// migrateMarker tracks an in-flight migration so a crash can resume or roll
// back instead of leaving a half-swapped directory.
type migrateMarker struct {
	Step         string `json:"step"`
	KeyBackup    string `json:"keyBackup"`
	ConfigBackup string `json:"configBackup,omitempty"`
}

func markerPath(layout datadir.Layout) string {
	return filepath.Join(layout.Dir, markerName)
}

func stagedPath(layout datadir.Layout) string {
	return filepath.Join(layout.Dir, stagedName)
}

// HasPending reports whether a migration marker exists (crash recovery).
func HasPending(layout datadir.Layout) bool {
	_, err := os.Stat(markerPath(layout))
	return err == nil
}

// NeedsMigration reports whether legacy files exist while the new layout is
// incomplete, or a migration marker needs resuming.
func NeedsMigration(layout datadir.Layout) bool {
	if HasPending(layout) {
		return true
	}
	if layout.HasInitMarkers() {
		return false
	}
	if _, err := os.Stat(layout.LegacyConfig); err == nil {
		return true
	}
	// Old envelope key.json without legacy config (e.g. manual split) also
	// counts: if key.json exists but info/settings do not, and the key parses
	// as an envelope, it needs migration.
	if _, err := os.Stat(layout.KeyPath); err == nil {
		return true
	}
	return false
}

// MigrateLegacy performs the legacy migration. password is required to verify
// the envelope HMAC; it is not persisted. The swap is staged: originals stay
// live until the final atomic rename, and a pending marker makes every crash
// point resumable or rollback-safe.
func MigrateLegacy(layout datadir.Layout, password string) error {
	if password == "" {
		return errors.New("migrate: 需要钱包密码以验证旧钱包")
	}
	if err := layout.EnsureDirs(); err != nil {
		return err
	}
	// Resume path first: a marker means a previous attempt did not finish.
	if HasPending(layout) {
		if err := recoverPending(layout, password); err != nil {
			return err
		}
		if layout.HasInitMarkers() && !HasPending(layout) {
			return nil
		}
		// Rolled back to originals; fall through to a fresh attempt below.
	}
	if layout.HasInitMarkers() {
		return errors.New("migrate: 新布局已存在，拒绝覆盖")
	}
	return migrateFresh(layout, password)
}

// recoverPending resumes or rolls back an interrupted migration. After it
// returns nil without a marker, the directory is either fully migrated or
// restored to its original files for a fresh attempt.
func recoverPending(layout datadir.Layout, password string) error {
	raw, err := os.ReadFile(markerPath(layout))
	if err != nil {
		return nil
	}
	var marker migrateMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		// Unreadable marker: drop it only if nothing staged anything new;
		// originals were never touched before the marker was written.
		_ = os.Remove(markerPath(layout))
		_ = os.Remove(stagedPath(layout))
		_ = os.Remove(layout.InfoPath)
		_ = os.Remove(layout.SettingsPath)
		return nil
	}
	// Case 1: the swap already happened (key.json is pure and consistent).
	// Finish the bookkeeping without touching key material.
	if isPureKey(layout.KeyPath) {
		if err := verifyPair(layout, password); err == nil {
			finishSwap(layout, marker)
			return nil
		}
		// Pure key but password does not verify it: do not destroy anything;
		// report and let the operator retry with the right password.
		return fmt.Errorf("migrate: 恢复时新钱包与密码不匹配，保留现场请重试: %w", err)
	}
	// Case 2: swap never happened; originals are intact (they are only
	// replaced by the final atomic rename). Drop staged/partial new files and
	// the marker, then let the caller run a fresh migration.
	_ = os.Remove(stagedPath(layout))
	_ = os.Remove(layout.InfoPath)
	_ = os.Remove(layout.SettingsPath)
	_ = os.Remove(markerPath(layout))
	return nil
}

// finishSwap runs the post-rename bookkeeping exactly once: remove the legacy
// config original (its backup is retained) and clear the marker.
func finishSwap(layout datadir.Layout, marker migrateMarker) {
	if marker.ConfigBackup != "" {
		_ = os.Remove(layout.LegacyConfig)
	}
	_ = os.Remove(stagedPath(layout))
	_ = os.Remove(markerPath(layout))
}

// isPureKey reports whether path holds an official KeyHold document.
func isPureKey(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_, err = keyhold.Parse(raw)
	return err == nil
}

// verifyPair checks the new key.json against info.json using password.
func verifyPair(layout datadir.Layout, password string) error {
	raw, err := os.ReadFile(layout.KeyPath)
	if err != nil {
		return err
	}
	doc, err := keyhold.Parse(raw)
	if err != nil {
		return fmt.Errorf("钱包文件无效: %w", err)
	}
	result, err := keyhold.Unlock(doc, password)
	if err != nil {
		return fmt.Errorf("密码错误或文件损坏: %w", err)
	}
	defer clearBytes(result.PrivateKey)
	store := appconfig.New(layout.Dir)
	if err := store.Load(); err != nil {
		return err
	}
	info, _, err := store.Snapshot()
	if err != nil {
		return err
	}
	if result.PublicKeyHex != info.PublicKeyHex {
		return errors.New("钱包公钥与身份元数据不一致")
	}
	expected := wallet.Authentication(result.PrivateKey, info.BsvNetwork, result.PublicKeyHex)
	if expected != info.NetworkAuthenticationHex {
		return errors.New("钱包网络身份认证失败")
	}
	return nil
}

// migrateFresh runs one full migration with a pending marker covering the
// transaction. Originals stay live until the final atomic rename.
func migrateFresh(layout datadir.Layout, password string) error {
	// Handle embedded-wallet legacy config first: split it into an envelope
	// key.json plus a wallet-free config. If a previous split already left a
	// valid envelope behind, continue with it instead of failing.
	if _, err := os.Stat(layout.LegacyConfig); err == nil {
		if _, err := config.MigrateEmbeddedWallet(layout.LegacyConfig); err != nil {
			if keyRaw, rerr := os.ReadFile(layout.KeyPath); rerr != nil {
				return fmt.Errorf("migrate: 拆分内嵌钱包失败: %w", err)
			} else if _, perr := wallet.Parse(keyRaw); perr != nil {
				return fmt.Errorf("migrate: 拆分内嵌钱包失败: %w", err)
			}
			// A valid envelope already exists; continue with it.
		}
	}
	// Load legacy settings if present (may be absent when only key.json remains).
	var legacy *config.Config
	if _, err := os.Stat(layout.LegacyConfig); err == nil {
		cfg, err := config.Load(layout.LegacyConfig)
		if err != nil {
			return fmt.Errorf("migrate: 读取旧配置失败: %w", err)
		}
		legacy = cfg
	}
	raw, err := os.ReadFile(layout.KeyPath)
	if err != nil {
		return fmt.Errorf("migrate: 读取旧钱包失败: %w", err)
	}
	envelope, err := wallet.Parse(raw)
	if err != nil {
		return fmt.Errorf("migrate: 旧钱包不是有效的网络绑定外壳: %w", err)
	}
	if legacy != nil {
		if err := config.VerifyPair(legacy, envelope); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	privateKey, publicKeyHex, err := envelope.Unlock(password)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer clearBytes(privateKey)
	if envelope.KeyHold.PublicKeyHex != publicKeyHex {
		return errors.New("migrate: 钱包公钥不一致")
	}
	network := envelope.BsvNetwork
	// Extract the inner pure KeyHold document verbatim.
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil {
		return fmt.Errorf("migrate: 解析旧钱包失败: %w", err)
	}
	inner, ok := outer["keyHold"]
	if !ok {
		return errors.New("migrate: 旧钱包缺少内部 KeyHold")
	}
	if _, err := keyhold.Parse(inner); err != nil {
		return fmt.Errorf("migrate: 内部 KeyHold 无效: %w", err)
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, inner, "", "  "); err != nil {
		return fmt.Errorf("migrate: 内部 KeyHold 无效: %w", err)
	}
	indented.WriteByte('\n')
	// Back up originals (they stay live until the final swap).
	stamp := time.Now().Unix()
	marker := migrateMarker{Step: markerStepParsed}
	if legacy != nil {
		marker.ConfigBackup = fmt.Sprintf("%s.bak-%d", layout.LegacyConfig, stamp)
		if err := copyFile(layout.LegacyConfig, marker.ConfigBackup); err != nil {
			return fmt.Errorf("migrate: 备份旧配置失败: %w", err)
		}
	}
	marker.KeyBackup = fmt.Sprintf("%s.envelope-bak-%d", layout.KeyPath, stamp)
	if err := copyFile(layout.KeyPath, marker.KeyBackup); err != nil {
		return fmt.Errorf("migrate: 备份旧钱包失败: %w", err)
	}
	encoded, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	if err := writeAtomic0600(markerPath(layout), append(encoded, '\n')); err != nil {
		return fmt.Errorf("migrate: 写入迁移标记失败: %w", err)
	}
	store := appconfig.New(layout.Dir)
	if err := store.Init(network, publicKeyHex, envelope.NetworkAuthenticationHex); err != nil {
		rollbackPartials(layout, marker)
		return fmt.Errorf("migrate: 初始化新布局失败: %w", err)
	}
	// Copy ordinary settings from the legacy config through the unified path.
	if legacy != nil {
		if err := store.Load(); err != nil {
			rollbackPartials(layout, marker)
			return err
		}
		_, settings, err := store.Snapshot()
		if err != nil {
			rollbackPartials(layout, marker)
			return err
		}
		if _, err := store.UpdateSettings(settings.Revision, func(next *appconfig.Settings) error {
			next.WOCAPIKey = legacy.WOCAPIKey
			next.FeeRateSatoshisPerKb = legacy.FeeRateSatoshisPerKb
			next.Scheduler.MaxBlockPriceSatoshis = legacy.Scheduler.MaxBlockPriceSatoshis
			next.Scheduler.PoolBlocks = legacy.Scheduler.PoolBlocks
			next.Scheduler.TotalSeats = legacy.Scheduler.TotalSeats
			next.Scheduler.FloatingSeats = legacy.Scheduler.FloatingSeats
			next.Scheduler.RequestTimeoutMs = legacy.Scheduler.RequestTimeoutMs
			next.Services.SatSuppliers = convertSuppliers(legacy.Services.SatSuppliers)
			next.Services.ArbitrationServices = legacy.Services.ArbitrationServices
			next.Server.DefaultSeedPriceSatoshis = legacy.Server.DefaultSeedPriceSatoshis
			next.Server.DefaultFullBlockPriceSatoshis = legacy.Server.DefaultFullBlockPriceSatoshis
			next.Server.ArbiterPublicKeys = legacy.Server.ArbiterPublicKeys
			if legacy.Server.Collector.Enabled || legacy.Server.Collector.Endpoint != "" {
				next.Server.Collector.Collectors = []appconfig.CollectorEntry{{
					ID: "legacy", Name: "旧配置",
					Endpoint: legacy.Server.Collector.Endpoint,
					Enabled:  legacy.Server.Collector.Enabled,
				}}
			}
			return nil
		}); err != nil {
			rollbackPartials(layout, marker)
			return fmt.Errorf("migrate: 迁移旧设置失败: %w", err)
		}
	}
	// Stage the pure KeyHold next to (not over) the live envelope, verify it,
	// then swap atomically. A crash before the rename leaves originals live.
	if err := writeAtomic0600(stagedPath(layout), indented.Bytes()); err != nil {
		rollbackPartials(layout, marker)
		return fmt.Errorf("migrate: 暂存纯 KeyHold 失败: %w", err)
	}
	staged, err := os.ReadFile(stagedPath(layout))
	if err != nil {
		rollbackPartials(layout, marker)
		return fmt.Errorf("migrate: 回读暂存失败: %w", err)
	}
	stagedDoc, err := keyhold.Parse(staged)
	if err != nil {
		rollbackPartials(layout, marker)
		return fmt.Errorf("migrate: 暂存 KeyHold 无效: %w", err)
	}
	stagedKey, err := keyhold.Unlock(stagedDoc, password)
	if err != nil {
		rollbackPartials(layout, marker)
		return fmt.Errorf("migrate: 暂存 KeyHold 解密失败: %w", err)
	}
	if stagedKey.PublicKeyHex != publicKeyHex {
		clearBytes(stagedKey.PrivateKey)
		rollbackPartials(layout, marker)
		return errors.New("migrate: 暂存钱包公钥不一致")
	}
	clearBytes(stagedKey.PrivateKey)
	if err := os.Rename(stagedPath(layout), layout.KeyPath); err != nil {
		rollbackPartials(layout, marker)
		return fmt.Errorf("migrate: 替换钱包失败: %w", err)
	}
	finishSwap(layout, marker)
	return nil
}

// rollbackPartials removes staged and derived new files after a failed fresh
// attempt. Originals (and their backups) are untouched.
func rollbackPartials(layout datadir.Layout, _ migrateMarker) {
	_ = os.Remove(stagedPath(layout))
	_ = os.Remove(layout.InfoPath)
	_ = os.Remove(layout.SettingsPath)
	_ = os.Remove(markerPath(layout))
}

func convertSuppliers(in []config.Supplier) []appconfig.Supplier {
	out := make([]appconfig.Supplier, 0, len(in))
	for _, s := range in {
		out = append(out, appconfig.Supplier{
			ID: s.ID, Name: s.Name, PublicKeyHex: s.PublicKeyHex,
			Multiaddrs: append([]string(nil), s.Multiaddrs...), Enabled: s.Enabled,
		})
	}
	return out
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeAtomic0600(dst, raw)
}

func writeAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func clearBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
