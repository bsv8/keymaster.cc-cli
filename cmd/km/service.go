package main

import (
	"fmt"
	"os"
	"strings"
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
