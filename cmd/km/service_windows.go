//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/daemonrun"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
	"github.com/bsv8/keymaster.cc-cli/internal/session"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// kmServiceName is the Windows service name.
const kmServiceName = "km"

type kmServiceHandler struct {
	dataDir  string
	webAdmin bool
}

// Execute runs the backend under the service host. The whole lifecycle
// (open → start → serve → orderly close) is owned by daemonrun with
// TrayNever: services never start the tray. A requested stop cancels the
// context and waits for the same bounded cleanup as the foreground daemon;
// any other backend exit is reported as a service failure.
func (h *kmServiceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	codeCh := make(chan int, 1)
	go func() {
		codeCh <- daemonrun.Run(ctx, daemonrun.Options{
			DataDir:  h.dataDir,
			WebAdmin: h.webAdmin,
			Tray:     daemonrun.TrayNever,
		})
	}()
	// SCM StartPending must resolve: report Running once the backend
	// answers, fail when it exits first, honor Stop while waiting.
	readyTick := time.NewTicker(500 * time.Millisecond)
	defer readyTick.Stop()
	readyTimeout := time.NewTimer(60 * time.Second)
	defer readyTimeout.Stop()
running:
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				return mapServiceExit(<-codeCh)
			}
		case code := <-codeCh:
			return mapServiceExit(code)
		case <-readyTick.C:
			if serviceBackendReady(h.dataDir) {
				break running
			}
		case <-readyTimeout.C:
			cancel()
			return mapServiceExit(<-codeCh)
		}
	}
	changes <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				return mapServiceExit(<-codeCh)
			}
		case code := <-codeCh:
			// Backend exit without a service request: failure, so SCM
			// recovery (restart) can apply instead of silent death.
			return mapServiceExit(code)
		}
	}
}

// mapServiceExit keeps the historic mapping: only a clean stop reads as
// success; any backend failure code becomes a service failure.
func mapServiceExit(code int) (bool, uint32) {
	if code == exitOK {
		return false, 0
	}
	return true, 1
}

// serviceBackendReady probes the just-started backend. False only means
// "not yet"; a dead backend is observed through codeCh, never here.
func serviceBackendReady(dataDir string) bool {
	base, err := session.ResolveBaseURL("", dataDir)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := kmapi.New(base, nil)
	st, err := client.Status(ctx)
	if err != nil {
		return false
	}
	return st.State != ""
}

// runAsWindowsServiceIfNeeded reports whether the process is running as a
// Windows service. When true it runs the daemon under the service host with
// the same locked-start + Close semantics and returns (true, exitCode).
func runAsWindowsServiceIfNeeded(dataDir string) (bool, int) {
	isSvc, err := svc.IsWindowsService()
	if err != nil || !isSvc {
		return false, 0
	}
	elog, _ := eventlog.Open(kmServiceName)
	if elog != nil {
		defer elog.Close()
	}
	webAdmin := true
	for _, a := range os.Args[1:] {
		if a == "--web-admin=false" {
			webAdmin = false
		} else if a == "--web-admin" || a == "--web-admin=true" {
			webAdmin = true
		}
	}
	handler := &kmServiceHandler{dataDir: dataDir, webAdmin: webAdmin}
	// svc.Run blocks until Stop/Shutdown; orderly shutdown is owned by
	// daemonrun inside the handler.
	if err := svc.Run(kmServiceName, handler); err != nil {
		return true, exitError
	}
	return true, exitOK
}

func serviceInstall(globalDataDir string, args []string) int {
	webAdmin, rest, err := splitWebAdminFlag(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km service install:", err)
		return exitUsage
	}
	dataDirFlag, _, _, err := parseServiceFlags(rest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km service install:", err)
		return exitUsage
	}
	dataDir := dataDirFlag
	if dataDir == "" {
		dataDir = globalDataDir
	}
	if dataDir == "" {
		fmt.Fprintln(os.Stderr, "km service install 需要 --data-dir（Windows 无默认 /var/lib/km）")
		return exitUsage
	}
	// Resolve to an absolute path before registering: the service starts
	// with its own working directory, so a relative path would resolve
	// against the wrong directory and miss the wallet.
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
	dataDir = layout.Dir
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	// SCM runs the long-lived daemon locked; CLI commands pick it via probe.
	// Services never start the tray (--no-tray); Web Admin follows the
	// recorded install flag.
	binPath := serviceBinPath(exe, dataDir, webAdmin)
	// Repeatable install: an existing registration is updated in place
	// (program location or data directory) instead of failing.
	if qout, qerr := exec.Command("sc.exe", "query", kmServiceName).CombinedOutput(); qerr == nil {
		state, serr := scState()
		if serr != nil {
			fmt.Fprintf(os.Stderr, "km: 查询已有服务状态失败: %v\n", serr)
			return exitError
		}
		wasRunning := state != "STOPPED"
		if wasRunning {
			fmt.Printf("检测到已注册且未停止的服务（%s），先停止再更新…\n", state)
			if sout, serr := exec.Command("sc.exe", "stop", kmServiceName).CombinedOutput(); serr != nil {
				fmt.Fprintf(os.Stderr, "km: 停止运行中的服务失败: %v: %s（未更新注册，请重试）\n", serr, strings.TrimSpace(string(sout)))
				return exitError
			}
			if err := waitScState("STOPPED", 45*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "km: %v（未更新注册，请重试）\n", err)
				return exitError
			}
		}
		if cout, cerr := exec.Command("sc.exe", "config", kmServiceName, "binPath=", binPath).CombinedOutput(); cerr != nil {
			fmt.Fprintf(os.Stderr, "km: 更新服务注册失败: %v: %s\n", cerr, strings.TrimSpace(string(cout)))
			if wasRunning {
				_, _ = exec.Command("sc.exe", "start", kmServiceName).CombinedOutput()
			}
			return exitError
		}
		_ = exec.Command("sc.exe", "description", kmServiceName, "Keymaster CLI daemon (locked start)").Run()
		fmt.Printf("已更新 Windows 服务 %q: %s\n", kmServiceName, binPath)
		if wasRunning {
			if sout, serr := exec.Command("sc.exe", "start", kmServiceName).CombinedOutput(); serr != nil {
				fmt.Fprintf(os.Stderr, "km: 更新后启动服务失败: %v: %s，请检查事件日志后手动启动\n", serr, strings.TrimSpace(string(sout)))
				return exitError
			}
			if err := waitScState("RUNNING", 30*time.Second); err != nil {
				fmt.Fprintf(os.Stderr, "km: %v\n", err)
				return exitError
			}
			fmt.Printf("Windows 服务 %q 已重新启动\n", kmServiceName)
		}
		fmt.Printf("数据目录: %s\n", dataDir)
		return exitOK
	} else if !isScMissing(string(qout)) {
		fmt.Fprintf(os.Stderr, "km: 查询服务注册失败: %s\n", strings.TrimSpace(string(qout)))
		return exitError
	}
	if out, err := exec.Command("sc.exe", "create", kmServiceName, "binPath=", binPath, "start=", "auto").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "km: sc create 失败: %v: %s\n", err, strings.TrimSpace(string(out)))
		return exitError
	}
	_ = exec.Command("sc.exe", "description", kmServiceName, "Keymaster CLI daemon (locked start)").Run()
	fmt.Printf("已安装 Windows 服务 %q: %s（程序、钱包、配置和业务数据保留；需先 km init 初始化）\n", kmServiceName, binPath)
	fmt.Printf("数据目录: %s\n", dataDir)
	return exitOK
}

func serviceUninstall(_ string, _ []string) int {
	if out, err := exec.Command("sc.exe", "stop", kmServiceName).CombinedOutput(); err != nil {
		if !isScNotRunning(string(out)) {
			fmt.Fprintf(os.Stderr, "km: 提示: 停止服务未成功（可能未运行）: %s\n", strings.TrimSpace(string(out)))
		}
	} else {
		// Wait for the bounded Close收尾 to finish before deleting the
		// service definition; deleting while STOP_PENDING can fail.
		if err := waitScState("STOPPED", 45*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "km: %v，继续删除服务定义\n", err)
		}
	}
	if out, err := exec.Command("sc.exe", "delete", kmServiceName).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "km: sc delete 失败: %v: %s\n", err, strings.TrimSpace(string(out)))
		return exitError
	}
	fmt.Printf("已卸载 Windows 服务 %q（程序、钱包、配置和业务数据均保留）\n", kmServiceName)
	return exitOK
}

func serviceStatus(globalDataDir string, _ []string) int {
	out, err := exec.Command("sc.exe", "query", kmServiceName).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 服务未安装或查询失败；可用 km daemon 手动启动")
		return exitError
	}
	base, rerr := session.ResolveBaseURL("", globalDataDir)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "km: 未配置 API 地址:", rerr)
		return exitOK
	}
	client := kmapi.New(base, nil)
	st, serr := client.Status(nil)
	if serr != nil {
		fmt.Fprintln(os.Stderr, "km: 后台 API 不可达:", serr)
		return exitError
	}
	fmt.Printf("后台 API: %s initialized=%v state=%v\n", base, st.Initialized, st.State)
	_ = dlock.ErrActive
	return exitOK
}

func serviceStart(_ string, _ []string) int {
	out, err := exec.Command("sc.exe", "start", kmServiceName).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		if isScAlreadyRunning(string(out)) {
			fmt.Printf("Windows 服务 %q 已在运行\n", kmServiceName)
		} else {
			fmt.Fprintf(os.Stderr, "km: 启动服务失败: %v\n", err)
			return exitError
		}
	}
	if err := waitScState("RUNNING", 30*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "km: %v\n", err)
		return exitError
	}
	fmt.Printf("Windows 服务 %q 已启动\n", kmServiceName)
	return exitOK
}

func serviceStop(_ string, _ []string) int {
	out, err := exec.Command("sc.exe", "stop", kmServiceName).CombinedOutput()
	fmt.Print(string(out))
	if err != nil {
		if isScNotRunning(string(out)) {
			fmt.Printf("Windows 服务 %q 已经停止\n", kmServiceName)
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "km: 停止服务失败: %v\n", err)
		return exitError
	}
	// The stop notification only starts the daemon's bounded Close收尾;
	// wait until the system reports STOPPED before returning.
	if err := waitScState("STOPPED", 45*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "km: %v\n", err)
		return exitError
	}
	fmt.Println("后台已停止（同一 Close 有界收尾完成）")
	return exitOK
}

func serviceRestart(_ string, args []string) int {
	if code := serviceStop("", args); code != exitOK {
		return code
	}
	return serviceStart("", args)
}

// scState parses the STATE line of `sc query` output.
func scState() (string, error) {
	out, err := exec.Command("sc.exe", "query", kmServiceName).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("查询服务状态失败: %s", strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "STATE") {
			// e.g. "STATE              : 4  RUNNING"
			fields := strings.Fields(trimmed)
			if len(fields) > 0 {
				return fields[len(fields)-1], nil
			}
		}
	}
	return "", fmt.Errorf("无法解析服务状态: %s", strings.TrimSpace(string(out)))
}

// waitScState polls until the service reaches want or timeout.
func waitScState(want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		state, err := scState()
		if err == nil && state == want {
			return nil
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("等待服务进入 %s 超时（当前 %q）", want, state)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func isScMissing(out string) bool {
	// 1060: 指定的服务未安装。
	return strings.Contains(out, "1060")
}

func isScNotRunning(out string) bool {
	// 1062: 服务尚未启动；1060: 服务不存在（视为已停止）。
	return strings.Contains(out, "1062") || strings.Contains(out, "1060")
}

func isScAlreadyRunning(out string) bool {
	// 1056: 服务已经在运行。
	return strings.Contains(out, "1056")
}

func activateUnitAfterInstall(_, _ bool, _, _, _, _ string, _ bool, _ string) int {
	fmt.Fprintln(os.Stderr, "km: 当前平台无 systemd，跳过服务激活；用 km daemon 手动运行")
	return exitOK
}

// privilegedInstallOrUpgrade is Linux-only; on Windows `km install`
// never reaches the privileged path, but it must compile.
func privilegedInstallOrUpgrade(layout datadir.Layout, exe, target, out string, webAdmin bool) int {
	if exe != target {
		if err := copyBinary(exe, target); err != nil {
			fmt.Fprintln(os.Stderr, "km: 安装程序失败:", err)
			return exitError
		}
	}
	unit := renderUnit(target, layout.Dir, webAdmin)
	if err := os.WriteFile(out, []byte(unit), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "km: 写入服务配置失败:", err)
		return exitError
	}
	fmt.Printf("已写入配置: %s 数据目录: %s\n", out, layout.Dir)
	return activateUnitAfterInstall(layout.HasInitMarkers(), layout.HasAnyWallet(), target, layout.Dir, "", "", false, out)
}

// serviceBinPath renders the SCM command line for the service backend.
func serviceBinPath(exe, dataDir string, webAdmin bool) string {
	return fmt.Sprintf("\"%s\" daemon --data-dir \"%s\" --no-tray%s", exe, dataDir, serviceExtraArgs(webAdmin))
}
