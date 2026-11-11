//go:build !linux && !windows && !darwin

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
)

func serviceInstall(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "km service install: 当前平台不支持系统服务安装，请用 km daemon --data-dir DIR 手动运行")
	return exitError
}

func serviceUninstall(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "km service uninstall: 当前平台不支持系统服务，请手动停止 km daemon（程序、钱包、配置和业务数据保留）")
	return exitError
}

func serviceStatus(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "km service status: 当前平台无系统服务管理；后台用 km daemon 手动运行，状态用 km info 查询")
	return exitError
}

func serviceStart(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "km service start: 当前平台不支持，请用 km daemon --data-dir DIR 手动启动")
	return exitError
}

func serviceStop(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "km service stop: 当前平台不支持，请用 Ctrl-C/SIGTERM 停止 km daemon（有界收尾）")
	return exitError
}

func serviceRestart(_ string, _ []string) int {
	fmt.Fprintln(os.Stderr, "km service restart: 当前平台不支持，请手动重启 km daemon")
	return exitError
}

func runAsWindowsServiceIfNeeded(_ string) (bool, int) { return false, 0 }

func activateUnitAfterInstall(_, _ bool, _, _, _, _ string, _ bool, _ string) int {
	fmt.Fprintln(os.Stderr, "km: 当前平台无系统服务激活；用 km daemon 手动运行")
	return exitOK
}

// privilegedInstallOrUpgrade is Linux-only; on other platforms `km install`
// never reaches the privileged path, but it must compile.
func privilegedInstallOrUpgrade(layout datadir.Layout, exe, target, out string) int {
	if exe != target {
		if err := copyBinary(exe, target); err != nil {
			fmt.Fprintln(os.Stderr, "km: 安装程序失败:", err)
			return exitError
		}
	}
	unit := renderUnit(target, layout.Dir)
	if err := os.WriteFile(out, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "km: 写入服务配置失败:", err)
		return exitError
	}
	fmt.Printf("已写入配置: %s 数据目录: %s\n", out, layout.Dir)
	return activateUnitAfterInstall(layout.HasInitMarkers(), layout.HasAnyWallet(), target, layout.Dir, "", "", false, out)
}
