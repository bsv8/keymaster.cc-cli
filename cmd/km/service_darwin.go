//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/session"
)

// macOS 服务经 launchd 管理，与 Linux systemd、Windows Service 保持同样的
// 安装、卸载、状态、启动、停止、重启语义：
//   - 服务锁定启动，不保存密码；安装升级保留用户数据；
//   - 卸载先停止再注销，保留程序、账户、钱包、配置与业务数据；
//   - 失败报告错误并恢复原程序和服务配置。
//
// 非 root 时安装为当前用户的 LaunchAgent；root 时安装为系统 LaunchDaemon。
const launchdLabel = "com.bsv8.km"

func launchdPlistPath() string {
	if os.Geteuid() == 0 {
		return "/Library/LaunchDaemons/" + launchdLabel + ".plist"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library/LaunchAgents", launchdLabel+".plist")
}

func launchdLogPath(name string) string {
	if os.Geteuid() == 0 {
		return filepath.Join("/var/log", launchdLabel+"."+name+".log")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library/Logs", launchdLabel+"."+name+".log")
}

// launchdDomain 返回 bootstrap/bootout/print 的 domain-target。
func launchdDomain() string {
	if os.Geteuid() == 0 {
		return "system"
	}
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func launchdPlist(target, dataDir string, webAdmin bool) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + target + `</string>
    <string>daemon</string>
    <string>--data-dir</string>
    <string>` + dataDir + `</string>
    <string>--no-tray</string>` + launchdWebAdminArg(webAdmin) + `
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>` + launchdLogPath("out") + `</string>
  <key>StandardErrorPath</key><string>` + launchdLogPath("err") + `</string>
</dict>
</plist>
`
}

func writeFileAtomicLocal(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*.plist")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func backupLivePlist(path, modeDesc string) (backup string, ok bool) {
	_ = modeDesc
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", true
		}
		fmt.Fprintln(os.Stderr, "km: 读取现服务配置失败:", err)
		return "", false
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bak-*.plist")
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 创建服务配置备份失败:", err)
		return "", false
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		fmt.Fprintln(os.Stderr, "km: 写入服务配置备份失败:", err)
		return "", false
	}
	tmp.Close()
	return name, true
}

// launchctlBootout 停止并注销服务（卸载先停止的语义）。
func launchctlBootout() {
	domain := launchdDomain()
	if out, err := exec.Command("launchctl", "bootout", domain+"/"+launchdLabel).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			fmt.Fprintf(os.Stderr, "km: 提示: launchctl bootout 未成功（%v: %s），继续\n", err, msg)
		}
	}
}

func serviceInstall(globalDataDir string, args []string) int {
	webAdmin, rest, err := splitWebAdminFlag(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km service install:", err)
		return exitUsage
	}
	dataDirFlag, plistOut, _, err := parseServiceFlags(rest)
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
	if err := rejectSharedDesktopDir(layout); err != nil {
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
	if os.Geteuid() != 0 {
		target = exe
	}
	out := plistOut
	if out == "" {
		out = launchdPlistPath()
	}
	if out == "" {
		fmt.Fprintln(os.Stderr, "km: 无法定位用户 LaunchAgents 目录")
		return exitError
	}
	// 受控停止旧服务，备份程序与配置，失败恢复。
	wasLoaded := launchdIsLoaded()
	if wasLoaded {
		launchctlBootout()
	}
	backup, plistBackup := "", ""
	defer func() {
		if backup != "" {
			_ = os.Remove(backup)
		}
		if plistBackup != "" {
			_ = os.Remove(plistBackup)
		}
	}()
	if wasLoaded && exe != target {
		var ok bool
		if backup, ok = backupLivePlist(target, "旧程序"); !ok {
			return exitError
		}
		if plistBackup, ok = backupLivePlist(out, "旧服务配置"); !ok {
			return exitError
		}
	}
	if exe != target {
		if err := copyBinary(exe, target); err != nil {
			fmt.Fprintln(os.Stderr, "km: 安装程序失败:", err)
			restoreLaunchdAfterFailure(target, backup, out, plistBackup)
			return exitError
		}
	}
	if err := writeFileAtomicLocal(out, []byte(launchdPlist(target, layout.Dir, webAdmin)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "km: 写入 launchd plist 失败:", err)
		restoreLaunchdAfterFailure(target, backup, out, plistBackup)
		return exitError
	}
	fmt.Printf("已写入 plist: %s 数据目录: %s\n", out, layout.Dir)
	if layout.HasInitMarkers() {
		fmt.Println("检测到已初始化钱包，升级保留钱包/info/settings 及全部业务数据。")
		if combined, err := exec.Command("launchctl", "bootstrap", launchdDomain(), out).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "km: launchctl bootstrap 未成功（%v: %s），请手动执行\n", err, strings.TrimSpace(string(combined)))
			restoreLaunchdAfterFailure(target, backup, out, plistBackup)
			return exitError
		}
		fmt.Println("已启动后台（仅启动 km daemon，锁定状态，需 km unlock）")
		return exitOK
	}
	if layout.HasAnyWallet() {
		fmt.Println("检测到旧版或半套数据，请先完成迁移或修复再启用服务：")
		fmt.Printf("  %s init --data-dir %s\n", target, layout.Dir)
		return exitOK
	}
	fmt.Println("尚未初始化，请先完成指定目录的初始化再启用服务：")
	fmt.Printf("  %s init --data-dir %s\n", target, layout.Dir)
	return exitOK
}

// restoreLaunchdAfterFailure 在升级失败时恢复原程序和服务配置。
func restoreLaunchdAfterFailure(target, backup, plistOut, plistBackup string) {
	if backup == "" && plistBackup == "" {
		return
	}
	fmt.Fprintln(os.Stderr, "km: 升级失败，正在恢复旧版本…")
	if backup != "" {
		if err := os.Rename(backup, target); err != nil {
			fmt.Fprintf(os.Stderr, "km: 恢复旧程序失败: %v，请手动处理 %s\n", err, target)
		}
	}
	if plistBackup != "" {
		if err := os.Rename(plistBackup, plistOut); err != nil {
			fmt.Fprintf(os.Stderr, "km: 恢复旧服务配置失败: %v，请手动处理 %s\n", err, plistOut)
		}
	}
	if out, err := exec.Command("launchctl", "bootstrap", launchdDomain(), plistOut).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "km: 恢复旧版本启动失败: %v: %s\n", err, strings.TrimSpace(string(out)))
		return
	}
	fmt.Fprintln(os.Stderr, "km: 已恢复旧版本并启动")
}

// launchdIsLoaded 报告服务是否已加载。
func launchdIsLoaded() bool {
	out, err := exec.Command("launchctl", "print", launchdDomain()+"/"+launchdLabel).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), launchdLabel)
}

func serviceUninstall(_ string, args []string) int {
	_, plistOut, _, err := parseServiceFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km service uninstall:", err)
		return exitUsage
	}
	out := plistOut
	if out == "" {
		out = launchdPlistPath()
	}
	if out == "" {
		fmt.Fprintln(os.Stderr, "km: 无法定位用户 LaunchAgents 目录")
		return exitError
	}
	// 先停止再注销；保留程序、账户、钱包、配置和业务数据。
	launchctlBootout()
	if err := os.Remove(out); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "km: plist 不存在: %s（程序、钱包、配置和业务数据均保留）\n", out)
			return exitOK
		}
		fmt.Fprintln(os.Stderr, "km: 删除 plist 失败:", err)
		return exitError
	}
	fmt.Printf("已卸载服务 plist: %s（程序、钱包、配置和业务数据均保留）\n", out)
	return exitOK
}

func serviceStatus(globalDataDir string, _ []string) int {
	out, err := exec.Command("launchctl", "print", launchdDomain()+"/"+launchdLabel).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 服务未加载（或无 launchd）；可用 km daemon 手动启动")
	}
	base, rerr := session.ResolveBaseURL("", globalDataDir)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "km: 未配置 API 地址:", rerr)
		if err != nil {
			return exitError
		}
		return exitOK
	}
	client := kmapi.New(base, nil)
	st, serr := client.Status(nil)
	if serr != nil {
		fmt.Fprintln(os.Stderr, "km: 后台 API 不可达:", serr)
		return exitError
	}
	fmt.Printf("后台 API: %s initialized=%v state=%v\n", base, st.Initialized, st.State)
	return exitOK
}

func serviceStart(_ string, _ []string) int {
	out := launchdPlistPath()
	if out == "" {
		fmt.Fprintln(os.Stderr, "km: 无法定位用户 LaunchAgents 目录")
		return exitError
	}
	if combined, err := exec.Command("launchctl", "bootstrap", launchdDomain(), out).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(combined))
		if strings.Contains(msg, "Already loaded") || strings.Contains(msg, "already") {
			fmt.Println("服务已在运行")
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "km: 启动服务失败: %v: %s\n", err, msg)
		return exitError
	}
	fmt.Println("已启动服务（仅启动 km daemon，锁定状态，需 km unlock）")
	return exitOK
}

func serviceStop(_ string, _ []string) int {
	launchctlBootout()
	if launchdIsLoaded() {
		fmt.Fprintln(os.Stderr, "km: 停止服务失败，仍在加载态")
		return exitError
	}
	fmt.Println("已停止服务")
	return exitOK
}

func serviceRestart(_ string, _ []string) int {
	if code := serviceStop("", nil); code != exitOK {
		return code
	}
	return serviceStart("", nil)
}

// privilegedInstallOrUpgrade is the macOS root install/upgrade flow:
// stop a loaded service, back up binary+plist, replace both, bootstrap
// with rollback. Repeatable; wallets and business data are never touched.
func privilegedInstallOrUpgrade(layout datadir.Layout, exe, target, _ string, webAdmin bool) int {
	out := launchdPlistPath()
	wasLoaded := launchdIsLoaded()
	if wasLoaded {
		launchctlBootout()
	}
	backup, plistBackup := "", ""
	defer func() {
		if backup != "" {
			_ = os.Remove(backup)
		}
		if plistBackup != "" {
			_ = os.Remove(plistBackup)
		}
	}()
	if wasLoaded && exe != target {
		var ok bool
		if backup, ok = backupLivePlist(target, "旧程序"); !ok {
			return exitError
		}
		if plistBackup, ok = backupLivePlist(out, "旧服务配置"); !ok {
			return exitError
		}
	}
	if exe != target {
		if err := copyBinary(exe, target); err != nil {
			fmt.Fprintln(os.Stderr, "km: 安装程序失败:", err)
			restoreLaunchdAfterFailure(target, backup, out, plistBackup)
			return exitError
		}
	}
	if err := writeFileAtomicLocal(out, []byte(launchdPlist(target, layout.Dir, webAdmin)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "km: 写入 launchd plist 失败:", err)
		restoreLaunchdAfterFailure(target, backup, out, plistBackup)
		return exitError
	}
	fmt.Printf("已写入 plist: %s 数据目录: %s\n", out, layout.Dir)
	if layout.HasInitMarkers() {
		fmt.Println("检测到已初始化钱包，升级保留钱包/info/settings 及全部业务数据。")
		if combined, err := exec.Command("launchctl", "bootstrap", launchdDomain(), out).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "km: launchctl bootstrap 未成功（%v: %s），请手动执行\n", err, strings.TrimSpace(string(combined)))
			restoreLaunchdAfterFailure(target, backup, out, plistBackup)
			return exitError
		}
		fmt.Println("已启动后台（仅启动 km daemon，锁定状态，需 km unlock）")
		return exitOK
	}
	if layout.HasAnyWallet() {
		fmt.Println("检测到旧版或半套数据，请先完成迁移或修复再启用服务：")
		fmt.Printf("  %s init --data-dir %s\n", target, layout.Dir)
		return exitOK
	}
	fmt.Println("尚未初始化，请先完成指定目录的初始化再启用服务：")
	fmt.Printf("  %s init --data-dir %s\n", target, layout.Dir)
	return exitOK
}

// Non-service entry: never intercepts `km daemon` on macOS.
func runAsWindowsServiceIfNeeded(_ string) (bool, int) { return false, 0 }

// launchdWebAdminArg renders the optional Web Admin opt-out for the plist.
// Services never start the tray (--no-tray is unconditional above).
func launchdWebAdminArg(webAdmin bool) string {
	if webAdmin {
		return ""
	}
	return `
    <string>--web-admin=false</string>`
}
