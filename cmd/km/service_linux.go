//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/apiclient"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
)

func defaultUnitPath() string { return "/etc/systemd/system/km.service" }

// systemctlIsActive reports whether the km unit is currently active.
func systemctlIsActive() bool {
	out, err := exec.Command("systemctl", "is-active", "km").CombinedOutput()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// waitSystemdActiveState polls ActiveState until it reaches want or timeout.
func waitSystemdActiveState(want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("systemctl", "show", "km", "-p", "ActiveState", "--value").CombinedOutput()
		if err == nil && strings.TrimSpace(string(out)) == want {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("等待服务状态 %q 超时", want)
		}
		time.Sleep(time.Second)
	}
}

// backupFile copies src to a temp file in the same directory for upgrade
// rollback. The caller removes it on success.
func backupFile(src string, mode os.FileMode) (string, error) {
	raw, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(src), ".km-prev-*.tmp")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, mode); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// stopServiceForUpgrade stops a running service before its binary is
// replaced. It returns wasActive=false with ok=true when there is nothing
// to stop (fresh install or verification output).
func stopServiceForUpgrade(unitOut string) (wasActive bool, ok bool) {
	if unitOut != defaultUnitPath() {
		return false, true
	}
	if !systemctlIsActive() {
		return false, true
	}
	fmt.Println("检测到运行中的后台，先停止再升级…")
	if combined, err := exec.Command("systemctl", "stop", "km").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "km: 停止运行中的后台失败: %v: %s（未替换程序，请检查 journalctl -u km 后重试）\n", err, strings.TrimSpace(string(combined)))
		return false, false
	}
	if err := waitSystemdActiveState("inactive", 40*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "km: %v（未替换程序，旧版本仍在收尾或已停止，请检查后重试）\n", err)
		return false, false
	}
	return true, true
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by an fsync and atomic rename. A failure (e.g. disk full) never
// truncates the existing file, so rollback always has a complete old unit
// to restore from.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".km-unit-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
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

// backupLiveFile copies an existing file to a temp backup. When the file
// does not exist there is nothing to back up (backup="", ok=true). On
// failure ok=false and the error is already printed.
func backupLiveFile(path string, mode os.FileMode, desc string) (backup string, ok bool) {
	if _, err := os.Stat(path); err != nil {
		return "", true
	}
	b, err := backupFile(path, mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: 备份%s失败: %v（未替换，请重试）\n", desc, err)
		return "", false
	}
	return b, true
}

// privilegedInstallOrUpgrade performs the root install steps shared by
// `km install` and `km service install`: it stops a running service, backs
// up the live binary and unit, replaces both, and activates the service.
// Wallets, settings and business data are never touched. Any failure rolls
// back to the previous version and returns a non-zero exit code.
func privilegedInstallOrUpgrade(layout datadir.Layout, exe, target, out string) int {
	wasActive, ok := stopServiceForUpgrade(out)
	if !ok {
		return exitError
	}
	backup, unitBackup := "", ""
	unitReplaced := false
	defer func() {
		if backup != "" {
			_ = os.Remove(backup)
		}
		if unitBackup != "" {
			_ = os.Remove(unitBackup)
		}
	}()
	if wasActive {
		var ok bool
		if backup, ok = backupLiveFile(target, 0o755, "旧程序"); !ok {
			_, _ = exec.Command("systemctl", "start", "km").CombinedOutput()
			return exitError
		}
		if unitBackup, ok = backupLiveFile(out, 0o644, "旧服务配置"); !ok {
			_, _ = exec.Command("systemctl", "start", "km").CombinedOutput()
			return exitError
		}
	}
	if exe != target {
		if err := copyBinary(exe, target); err != nil {
			fmt.Fprintln(os.Stderr, "km: 安装程序失败:", err)
			restoreAfterFailure(target, backup, out, unitBackup, unitReplaced)
			return exitError
		}
	}
	unit := strings.ReplaceAll(unitTemplate, "@DATA_DIR@", layout.Dir)
	unit = strings.ReplaceAll(unit, "@KM_BIN@", target)
	if err := writeFileAtomic(out, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "km: 写入 systemd unit 失败:", err)
		restoreAfterFailure(target, backup, out, unitBackup, unitReplaced)
		return exitError
	}
	fmt.Printf("已写入 unit: %s 数据目录: %s\n", out, layout.Dir)
	unitReplaced = unitBackup != ""
	return activateUnitAfterInstall(layout.HasInitMarkers(), layout.HasAnyWallet(), target, layout.Dir, backup, unitBackup, unitReplaced, out)
}

func serviceInstall(globalDataDir string, args []string) int {
	dataDirFlag, unitOut, _, err := parseServiceFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km service install:", err)
		return exitUsage
	}
	dataDir := dataDirFlag
	if dataDir == "" {
		dataDir = globalDataDir
	}
	if dataDir == "" {
		if os.Geteuid() == 0 {
			dataDir = "/var/lib/km"
		} else {
			dataDir = ""
		}
	}
	layout, err := datadir.Resolve(dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if err := layout.EnsureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	target := "/usr/local/bin/km"
	isRoot := os.Geteuid() == 0
	out := unitOut
	if out == "" {
		out = defaultUnitPath()
	}
	if !isRoot && out == defaultUnitPath() {
		fmt.Fprintln(os.Stderr, "km service install 需要 root（或用 --unit-out 指定输出验证）")
		return exitError
	}
	if isRoot {
		if err := ensureServiceUser(layout.Dir); err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		return privilegedInstallOrUpgrade(layout, exe, target, out)
	}
	// Non-root verification path: write the unit for inspection only.
	// Activation below warns without systemd and keeps exit 0.
	target = exe
	unit := strings.ReplaceAll(unitTemplate, "@DATA_DIR@", layout.Dir)
	unit = strings.ReplaceAll(unit, "@KM_BIN@", target)
	if err := writeFileAtomic(out, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "km: 写入 systemd unit 失败:", err)
		return exitError
	}
	fmt.Printf("已写入 unit: %s 数据目录: %s\n", out, layout.Dir)
	return activateUnitAfterInstall(layout.HasInitMarkers(), layout.HasAnyWallet(), target, layout.Dir, "", "", false, out)
}

func serviceUninstall(_ string, args []string) int {
	_, unitOut, _, err := parseServiceFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km service uninstall:", err)
		return exitUsage
	}
	out := unitOut
	if out == "" {
		out = defaultUnitPath()
	}
	if os.Geteuid() != 0 && out == defaultUnitPath() {
		fmt.Fprintln(os.Stderr, "km service uninstall 需要 root（或用 --unit-out 指定要删除的文件做验证）")
		return exitError
	}
	// Stop and disable without deleting wallets, config or business data.
	if out == defaultUnitPath() {
		if combined, err := exec.Command("systemctl", "disable", "--now", "km").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "km: 提示: systemctl disable --now km 未成功（%v: %s），继续删除 unit\n", err, strings.TrimSpace(string(combined)))
		} else {
			fmt.Printf("已停止并禁用服务: %s\n", strings.TrimSpace(string(combined)))
		}
	}
	if err := os.Remove(out); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "km: unit 不存在: %s（程序、钱包、配置和业务数据均保留）\n", out)
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "km: 删除 unit 失败:", err)
		return exitError
	}
	if out == defaultUnitPath() {
		if combined, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "km: 提示: systemctl daemon-reload 未成功（%v: %s）\n", err, strings.TrimSpace(string(combined)))
		}
	}
	fmt.Printf("已卸载服务 unit: %s（程序、钱包、配置和业务数据均保留）\n", out)
	return exitOK
}

func serviceStatus(globalDataDir string, _ []string) int {
	// systemd state first.
	activeOut, activeErr := exec.Command("systemctl", "is-active", "km").CombinedOutput()
	enabledOut, _ := exec.Command("systemctl", "is-enabled", "km").CombinedOutput()
	fmt.Printf("systemd active: %s\n", strings.TrimSpace(string(activeOut)))
	fmt.Printf("systemd enabled: %s\n", strings.TrimSpace(string(enabledOut)))
	if activeErr != nil {
		fmt.Fprintln(os.Stderr, "km: 服务未运行（或无 systemd），可用 km daemon 手动启动；查看 journalctl -u km")
	}
	// Then daemon API when reachable.
	base, err := apiclient.ResolveBaseURL("", globalDataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 未配置 API 地址:", err)
		if activeErr != nil {
			return exitError
		}
		return exitOK
	}
	client := apiclient.NewExternal(base)
	st, err := client.Status(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 后台 API 不可达:", err)
		return exitError
	}
	fmt.Printf("后台 API: %s initialized=%v state=%v network=%v locked=%v\n",
		base, st["initialized"], st["state"], st["network"], st["locked"])
	return exitOK
}

func runSystemctl(action string) int {
	out, err := exec.Command("systemctl", action, "km").CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if trimmed != "" {
		fmt.Println(trimmed)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: systemctl %s km 失败: %v: %s\n", action, err, trimmed)
		return exitError
	}
	fmt.Printf("已执行: systemctl %s km\n", action)
	return exitOK
}

// restoreAfterFailure rolls the binary and unit back after a failed upgrade
// of a running service, then verifies the previous version actually starts
// again. Best effort: every step reports, never silently leaves a stopped
// service behind.
func restoreAfterFailure(target, backup, unitOut, unitBackup string, unitReplaced bool) {
	if unitOut != defaultUnitPath() {
		return
	}
	if backup == "" && !(unitBackup != "" && unitReplaced) {
		return
	}
	fmt.Fprintln(os.Stderr, "km: 升级失败，正在恢复旧版本…")
	if backup != "" {
		if err := os.Rename(backup, target); err != nil {
			fmt.Fprintf(os.Stderr, "km: 恢复旧程序失败: %v，请手动处理 %s\n", err, target)
		}
	}
	if unitBackup != "" && unitReplaced {
		if err := os.Rename(unitBackup, unitOut); err != nil {
			fmt.Fprintf(os.Stderr, "km: 恢复旧服务配置失败: %v，请手动处理 %s\n", err, unitOut)
		}
	}
	_, _ = exec.Command("systemctl", "daemon-reload").CombinedOutput()
	if out, err := exec.Command("systemctl", "start", "km").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "km: 恢复旧版本启动失败: %v: %s，请检查 journalctl -u km\n", err, strings.TrimSpace(string(out)))
		return
	}
	if err := waitSystemdActiveState("active", 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "km: 恢复后服务未进入 active: %v，请检查 journalctl -u km\n", err)
		return
	}
	fmt.Fprintln(os.Stderr, "km: 已恢复旧版本并启动")
}

// activateUnitAfterInstall is the single Linux place that activates the
// service after the binary and unit were written. backup/unitBackup carry
// the pre-upgrade files ("" when nothing was replaced); failures roll back
// and return a non-zero exit code.
func activateUnitAfterInstall(layoutHasInit, layoutHasWallet bool, target, dataDir, backup, unitBackup string, unitReplaced bool, unitOut string) int {
	if layoutHasInit {
		fmt.Println("检测到已初始化钱包，升级保留钱包/info/settings 及全部业务数据。")
		if combined, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "km: daemon-reload 失败: %v: %s\n", err, strings.TrimSpace(string(combined)))
			if backup != "" || unitBackup != "" {
				restoreAfterFailure(target, backup, unitOut, unitBackup, unitReplaced)
				return exitError
			}
			if unitOut != defaultUnitPath() {
				// Verification output only: warn and succeed.
				return exitOK
			}
			fmt.Fprintln(os.Stderr, "km: 请手动执行 systemctl daemon-reload && systemctl enable --now km")
			return exitError
		}
		// A running daemon keeps executing the old binary after its file
		// is replaced; restart so the new version takes over. (In the
		// shared upgrade flow the service was already stopped, so this
		// branch only triggers for out-of-band activity.)
		if systemctlIsActive() {
			fmt.Println("检测到运行中的后台，重启以生效新版本…")
			if combined, err := exec.Command("systemctl", "restart", "km").CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "km: 重启后台失败: %v: %s\n", err, strings.TrimSpace(string(combined)))
				restoreAfterFailure(target, backup, unitOut, unitBackup, unitReplaced)
				return exitError
			}
			if err := waitSystemdActiveState("active", 45*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "km: %v，请检查 journalctl -u km\n", err)
				restoreAfterFailure(target, backup, unitOut, unitBackup, unitReplaced)
				return exitError
			}
			fmt.Println("已重启后台（仅启动 km daemon，锁定状态，需 km unlock）")
			return exitOK
		}
		if combined, err := exec.Command("systemctl", "enable", "--now", "km").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "km: systemctl enable --now km 未成功（%v: %s），请手动执行: systemctl enable --now km\n", err, strings.TrimSpace(string(combined)))
			restoreAfterFailure(target, backup, unitOut, unitBackup, unitReplaced)
			return exitError
		}
		if err := waitSystemdActiveState("active", 30*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "km: 后台启动后未进入 active: %v，请检查 journalctl -u km\n", err)
			restoreAfterFailure(target, backup, unitOut, unitBackup, unitReplaced)
			return exitError
		}
		fmt.Println("已执行: systemctl daemon-reload && systemctl enable --now km（仅启动 km daemon，锁定状态，需 km unlock）")
		return exitOK
	}
	if layoutHasWallet {
		fmt.Println("检测到旧版或半套数据，请先完成迁移或修复再启用服务：")
		fmt.Printf("  sudo -u km %s init --data-dir %s\n", target, dataDir)
		return exitOK
	}
	fmt.Println("尚未初始化，请先完成指定目录的初始化再启用服务：")
	fmt.Printf("  sudo -u km %s init --data-dir %s\n", target, dataDir)
	return exitOK
}

func serviceStart(_ string, _ []string) int { return runSystemctl("start") }

func serviceStop(_ string, _ []string) int { return runSystemctl("stop") }

func serviceRestart(_ string, _ []string) int { return runSystemctl("restart") }

// Non-service entry: never intercepts `km daemon` on Linux.
func runAsWindowsServiceIfNeeded(_ string) (bool, int) { return false, 0 }
