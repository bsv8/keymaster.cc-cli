package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
)

//go:embed km.service.tmpl
var unitTemplate string

func cmdInstall(g globalFlags, args []string) int {
	dataDirFlag := ""
	unitOut := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--data-dir" && i+1 < len(args):
			dataDirFlag = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--data-dir="):
			dataDirFlag = strings.TrimPrefix(args[i], "--data-dir=")
		case args[i] == "--unit-out" && i+1 < len(args):
			unitOut = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--unit-out="):
			unitOut = strings.TrimPrefix(args[i], "--unit-out=")
		default:
			fmt.Fprintf(os.Stderr, "km install 未知参数 %q（支持 --data-dir/--unit-out）\n", args[i])
			return exitUsage
		}
	}
	// --data-dir is accepted both as a global flag (km --data-dir DIR
	// install) and as a subcommand flag (km install --data-dir DIR).
	dataDir := dataDirFlag
	if dataDir == "" {
		dataDir = g.dataDir
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
	if os.Geteuid() != 0 {
		// Manual run: no system install, just verify the data dir.
		fmt.Printf("数据目录就绪: %s（当前用户安装，不写系统 unit；后台用 km daemon --data-dir %s 启动）\n", layout.Dir, layout.Dir)
		fmt.Printf("当前程序: %s\n", exe)
		return exitOK
	}
	// System install as root: create the service account, hand the data
	// directory to it, then run the shared privileged flow (stop a running
	// service, back up binary+unit, replace both, activate with rollback).
	// Repeatable: key/info/settings/business data are never touched.
	// macOS root goes through the launchd flow instead (no useradd/systemd).
	if runtime.GOOS == "darwin" && os.Geteuid() == 0 {
		return privilegedInstallOrUpgrade(layout, exe, "/usr/local/bin/km", "")
	}
	if err := ensureServiceUser(layout.Dir); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	out := unitOut
	if out == "" {
		out = "/etc/systemd/system/km.service"
	}
	return privilegedInstallOrUpgrade(layout, exe, target, out)
}

// ensureServiceUser creates the km service account/group when missing and
// hands the data directory to it (0700, km:km, recursively). Existing
// wallets, settings and business data are never modified, only ownership
// normalized (files created by a root-run init/upgrade would otherwise stay
// unreadable for the service user).
func ensureServiceUser(dataDir string) error {
	if _, err := user.Lookup("km"); err != nil {
		cmd := exec.Command("useradd", "--system", "--no-create-home",
			"--home-dir", dataDir, "--shell", "/usr/sbin/nologin", "km")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("创建 km 服务用户失败: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if _, err := user.LookupGroup("km"); err != nil {
			// useradd --system normally creates the group; not fatal.
			fmt.Fprintf(os.Stderr, "km: 警告: km 组缺失: %v\n", err)
		}
	}
	u, err := user.Lookup("km")
	if err != nil {
		return fmt.Errorf("km 服务用户不可用: %w", err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return fmt.Errorf("km 用户 UID 非法: %w", err)
	}
	gid := -1
	if g, err := user.LookupGroup("km"); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	}
	spec := fmt.Sprintf("%d:", uid)
	if gid >= 0 {
		spec = fmt.Sprintf("%d:%d", uid, gid)
	}
	out, err := exec.Command("chown", "-R", spec, dataDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("设置数据目录归属失败: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return fmt.Errorf("设置数据目录权限失败: %w", err)
	}
	return nil
}

func copyBinary(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".km-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return err
	}
	// System binaries stay root-owned: a service account must never be able
	// to rewrite the program it runs as (privilege escalation on the next
	// root-run `km install`). Only the data directory belongs to km.
	if err := os.Chown(name, 0, 0); err != nil {
		// Non-root dry runs (e.g. --unit-out checks): keep the file.
		fmt.Fprintf(os.Stderr, "km: 提示: 无法确认程序归属 root（%v），保留当前归属\n", err)
	}
	return os.Rename(name, dst)
}
