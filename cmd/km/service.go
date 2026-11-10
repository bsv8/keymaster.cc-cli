package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
)

func cmdService(g globalFlags, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: km service <install|uninstall|status|start|stop|restart> [--data-dir DIR]")
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	// Allow --data-dir as global or subcommand flag; service helpers re-parse.
	dataDir := g.dataDir
	// Extract --data-dir/--unit-out for install/uninstall without consuming
	// other flags; helpers validate the rest.
	switch sub {
	case "install":
		return serviceInstall(dataDir, rest)
	case "uninstall":
		return serviceUninstall(dataDir, rest)
	case "status":
		return serviceStatus(dataDir, rest)
	case "start":
		return serviceStart(dataDir, rest)
	case "stop":
		return serviceStop(dataDir, rest)
	case "restart":
		return serviceRestart(dataDir, rest)
	default:
		fmt.Fprintf(os.Stderr, "km: 未知 service 子命令 %q\n", sub)
		fmt.Fprintln(os.Stderr, "用法: km service <install|uninstall|status|start|stop|restart> [--data-dir DIR]")
		return exitUsage
	}
}

func parseServiceFlags(args []string) (dataDir, unitOut string, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--data-dir" && i+1 < len(args):
			dataDir = args[i+1]
			i++
		case strings.HasPrefix(a, "--data-dir="):
			dataDir = strings.TrimPrefix(a, "--data-dir=")
		case a == "--unit-out" && i+1 < len(args):
			unitOut = args[i+1]
			i++
		case strings.HasPrefix(a, "--unit-out="):
			unitOut = strings.TrimPrefix(a, "--unit-out=")
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 0 {
		err = fmt.Errorf("未知参数 %q", strings.Join(rest, " "))
	}
	return dataDir, unitOut, rest, err
}

// splitWebAdminFlag extracts --web-admin[=true|false] (default true,
// matching `km daemon`). Services accept it at install time so all three
// platforms record the same setting; the tray is never enabled for
// services regardless of flags.
func splitWebAdminFlag(args []string) (webAdmin bool, rest []string, err error) {
	webAdmin = true
	for _, a := range args {
		switch {
		case a == "--web-admin" || a == "--web-admin=true":
			webAdmin = true
		case a == "--web-admin=false":
			webAdmin = false
		case strings.HasPrefix(a, "--web-admin="):
			return false, nil, fmt.Errorf("未知参数 %q", a)
		default:
			rest = append(rest, a)
		}
	}
	return webAdmin, rest, nil
}

// serviceExtraArgs renders the daemon flags a service always carries:
// never the tray, plus --web-admin=false only when disabled.
func serviceExtraArgs(webAdmin bool) string {
	if webAdmin {
		return ""
	}
	return " --web-admin=false"
}

// renderUnit renders the systemd template with the service daemon flags.
func renderUnit(bin, dataDir string, webAdmin bool) string {
	unit := strings.ReplaceAll(unitTemplate, "@DATA_DIR@", dataDir)
	unit = strings.ReplaceAll(unit, "@KM_BIN@", bin)
	unit = strings.ReplaceAll(unit, "@EXTRA_ARGS@", serviceExtraArgs(webAdmin))
	return unit
}

// rejectSharedDesktopDir refuses a service data directory that equals the
// desktop default (~/.km of the invoking user). Service and desktop must
// not share a data directory: only one backend instance may own a
// directory, and a service wallet must not be the interactive one.
func rejectSharedDesktopDir(layout datadir.Layout) error {
	desktop, err := datadir.Resolve("")
	if err != nil {
		return nil
	}
	if layout.Dir == desktop.Dir {
		return fmt.Errorf("服务与桌面不得共用数据目录 %s，请用 --data-dir 指定服务专用目录", layout.Dir)
	}
	return nil
}
