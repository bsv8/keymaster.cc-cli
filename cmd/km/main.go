// Command km is the single Keymaster binary: installer, background daemon,
// CLI and embedded Web UI.
//
// New model:
//
//	km install [--data-dir DIR]     install binary + systemd unit (repeatable)
//	km init [--data-dir DIR]        initialize wallet + settings (network last)
//	km daemon [--data-dir DIR]      run background, locked start, serves HTTP
//	km unlock [--api URL]           hidden TTY password, shared unlock flow
//	km info [--api URL]             network, lock, address, QR, balance
//	km get <seed> [--api URL]       submit task, stream progress, disconnect safe
//
// The daemon is the sole owner. CLI/Web never write persistent files directly
// and never read private keys. Old -server/-out/-admin/-config entry points
// are retired: they exit with usage guidance instead of starting a second
// execution mode.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	keyhold "github.com/bsv8/KeyHold/go/v2"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"golang.org/x/term"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/daemonrun"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
	"github.com/bsv8/keymaster.cc-cli/internal/migrate"
	"github.com/bsv8/keymaster.cc-cli/internal/session"
	"github.com/bsv8/keymaster.cc-cli/internal/tray"
)

const (
	exitOK              = 0
	exitError           = 1
	exitUsage           = 2
	exitIncompleteFunds = 3
	exitCancelled       = 4
)

func main() {
	configureDebug()
	os.Exit(run(os.Args[1:]))
}

type globalFlags struct {
	dataDir string
	api     string
}

func parseGlobal(args []string) (globalFlags, []string, error) {
	var g globalFlags
	rest := []string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--data-dir" && i+1 < len(args) {
			g.dataDir = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--data-dir=") {
			g.dataDir = strings.TrimPrefix(a, "--data-dir=")
			continue
		}
		if a == "--api" && i+1 < len(args) {
			g.api = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(a, "--api=") {
			g.api = strings.TrimPrefix(a, "--api=")
			continue
		}
		if a == "--help" || a == "-h" {
			return g, []string{"help"}, nil
		}
		rest = append(rest, a)
	}
	return g, rest, nil
}

func run(args []string) int {
	// Retire old execution modes explicitly.
	for _, a := range args {
		switch {
		case a == "-server" || strings.HasPrefix(a, "-server=") ||
			a == "-out" || strings.HasPrefix(a, "-out=") ||
			a == "-admin" || strings.HasPrefix(a, "-admin=") ||
			a == "-config" || strings.HasPrefix(a, "-config=") ||
			a == "-listen" || strings.HasPrefix(a, "-listen=") ||
			a == "-discover" || strings.HasPrefix(a, "-discover=") ||
			a == "-legacy-direct" || a == "-discover=false":
			fmt.Fprintln(os.Stderr, "km: 旧入口已退役，请使用新命令：km install | init | daemon | unlock | info | get <seed>")
			usage(os.Stderr)
			return exitUsage
		case a == "-h" || a == "--help":
			// handled below
		}
	}
	g, rest, err := parseGlobal(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitUsage
	}
	if len(rest) == 0 {
		usage(os.Stderr)
		return exitUsage
	}
	cmd, cmdArgs := rest[0], rest[1:]
	switch cmd {
	case "help":
		usage(os.Stdout)
		return exitOK
	case "install":
		return cmdInstall(g, cmdArgs)
	case "init":
		return cmdInit(g, cmdArgs)
	case "daemon":
		return cmdDaemon(g, cmdArgs)
	case "internal-tray":
		return cmdInternalTray()
	case "unlock":
		return cmdUnlock(g, cmdArgs)
	case "info":
		return cmdInfo(g, cmdArgs)
	case "get":
		return cmdGet(g, cmdArgs)
	case "service":
		return cmdService(g, cmdArgs)
	default:
		// A bare seed hash without subcommand is the old download mode.
		if isSeedHash(cmd) && len(cmdArgs) == 0 {
			fmt.Fprintln(os.Stderr, "km: 旧下载入口已退役，请先启动后台再使用：km daemon / km get <seed>")
			usage(os.Stderr)
			return exitUsage
		}
		fmt.Fprintf(os.Stderr, "km: 未知命令 %q\n", cmd)
		usage(os.Stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `用法:
  km install [--data-dir DIR]   安装程序、检查初始化状态、生成系统服务
  km init [--data-dir DIR]      独立初始化钱包和设置，不启动长期业务
  km daemon [--data-dir DIR] [--no-tray]
                         运行后台，已有钱包时以锁定状态启动；默认启用 Web Admin 与托盘
  km unlock [--api URL] [--data-dir DIR]
  km info [--api URL] [--data-dir DIR]
  km get <seedhashhex> [--api URL] [--data-dir DIR]
  km service <install|uninstall|status|start|stop|restart> [--data-dir DIR]

全局选项:
  --data-dir DIR   数据目录（默认 ~/.km；系统服务建议 /var/lib/km）
  --api URL        后台 HTTP 地址，如 http://127.0.0.1:8790（默认读本机 settings.json，无后台时本地命令内嵌启动）

说明:
  后台仅监听 127.0.0.1，无 token；远程访问由用户反向代理与认证负责，远程解锁请用 HTTPS。
  缺少初始化数据时 daemon 提示执行 init，不在服务进程中等待交互。
  本地 info/get 无外部后台时内嵌启动（需密码+独占锁），退出即关闭；unlock 仅用于外部后台。`)
}

func isSeedHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ttyReader returns a reader for interactive prompts: /dev/tty when stdin is
// piped (e.g. curl | sh install), otherwise stdin. Without a terminal it
// returns an error instead of silently using empty passwords.
func ttyReader() (*os.File, func(), error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return os.Stdin, func() {}, nil
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, errors.New("没有交互终端，结束引导（请在终端中运行）")
	}
	return f, func() { f.Close() }, nil
}

func promptLine(prompt string) (string, error) {
	tty, closeTTY, err := ttyReader()
	if err != nil {
		return "", err
	}
	defer closeTTY()
	fmt.Fprint(os.Stderr, prompt)
	reader := bufio.NewReader(tty)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func readHiddenPassword(prompt string) (string, error) {
	return readHiddenPasswordWithContext(context.Background(), prompt)
}

// readHiddenPasswordWithContext reads a hidden password from the terminal
// (stdin when it is a TTY, else /dev/tty) and honors ctx cancellation
// (Ctrl-C): on cancel it restores the terminal state, moves to a new line
// and returns ctx.Err() so the caller can close any just-opened daemon and
// release the data directory lock instead of hanging on input.
func readHiddenPasswordWithContext(ctx context.Context, prompt string) (string, error) {
	// Never read from args, env, config or systemd credentials.
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	fd := int(os.Stdin.Fd())
	var tty *os.File
	if term.IsTerminal(fd) {
		tty = nil
	} else {
		var err error
		tty, err = os.OpenFile("/dev/tty", os.O_RDONLY, 0)
		if err != nil {
			return "", errors.New("没有交互终端，无法读取密码")
		}
		defer tty.Close()
		fd = int(tty.Fd())
	}
	// Best-effort terminal state for restoration on cancellation. The
	// blocked ReadPassword below restores it on return; when we give up
	// early we restore it ourselves so the shell is not left in raw mode.
	oldState, stateErr := term.GetState(fd)
	fmt.Fprint(os.Stderr, prompt)
	type result struct {
		value string
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		value, err := term.ReadPassword(fd)
		if err != nil {
			ch <- result{"", err}
			return
		}
		ch <- result{strings.TrimRight(string(value), "\r\n"), nil}
	}()
	select {
	case <-ctx.Done():
		if stateErr == nil {
			_ = term.Restore(fd, oldState)
		}
		fmt.Fprintln(os.Stderr)
		// The reader goroutine stays blocked until the next Enter and
		// then exits (buffered channel, process is leaving anyway).
		return "", ctx.Err()
	case res := <-ch:
		fmt.Fprintln(os.Stderr)
		if res.err != nil {
			return "", res.err
		}
		return res.value, nil
	}
}

func cmdInit(g globalFlags, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "km init 不接受参数")
		return exitUsage
	}
	layout, err := session.ResolveLayout(g.dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	guard, err := dlock.Acquire(layout.Dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 数据目录已有后台在运行，init 与后台互斥")
		return exitError
	}
	defer guard.Release()
	if err := layout.EnsureDirs(); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if layout.HasInitMarkers() && !migrate.HasPending(layout) {
		fmt.Fprintln(os.Stderr, "km: 已经初始化（存在 key.json/info.json/settings.json），不覆盖")
		return exitError
	}
	if migrate.NeedsMigration(layout) {
		// Legacy migration path (also resumes an interrupted migration).
		if migrate.HasPending(layout) {
			fmt.Fprintln(os.Stderr, "检测到未完成的迁移，继续恢复（验证后原样保留密文，不重加密）。")
		} else if _, err := os.Stat(layout.LegacyConfig); err == nil {
			fmt.Fprintln(os.Stderr, "检测到旧版数据（config.json），开始迁移（验证后原样保留密文，不重加密）。")
		} else {
			fmt.Fprintln(os.Stderr, "检测到旧钱包外壳，开始迁移。")
		}
		password, err := readHiddenPassword("钱包密码: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		if password == "" {
			fmt.Fprintln(os.Stderr, "km: 密码不能为空")
			return exitError
		}
		if err := migrate.MigrateLegacy(layout, password); err != nil {
			fmt.Fprintln(os.Stderr, "km: 迁移失败:", err)
			return exitError
		}
		fmt.Println("迁移完成：key.json 已为纯 KeyHold，网络绑定移入 info.json")
		return exitOK
	}
	if layout.HasAnyWallet() {
		fmt.Fprintln(os.Stderr, "km: 数据目录存在半套或损坏文件，不能当成首次安装；请从备份恢复后重试")
		return exitError
	}
	// Fresh init: new key or import, password, other settings (defaults), network last.
	fmt.Fprintln(os.Stderr, "初始化新钱包（网络最后选择）。")
	choice, err := promptLine("新建 key 还是导入 KeyHold？输入 new/import: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	var importedRaw []byte
	var privateKey []byte
	var publicKeyHex string
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "new", "n", "":
		first, err := readHiddenPassword("设置钱包密码: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		if first == "" {
			fmt.Fprintln(os.Stderr, "km: 密码不能为空")
			return exitError
		}
		second, err := readHiddenPassword("再次输入密码: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		if first != second {
			fmt.Fprintln(os.Stderr, "km: 两次密码不一致")
			return exitError
		}
		key, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 生成私钥失败:", err)
			return exitError
		}
		serialized := key.Serialize()
		// Export pure KeyHold now; network binding comes after network choice.
		raw, err := keyhold.ExportPrivateKey(keyhold.PrivateKeyExportInput{
			PrivateKey: serialized,
			Password:   first,
			Label:      "km wallet",
			Parameters: keyhold.RecommendedParameters(),
		})
		for i := range serialized {
			serialized[i] = 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 创建 KeyHold 失败:", err)
			return exitError
		}
		doc, err := keyhold.Parse(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 解析 KeyHold 失败:", err)
			return exitError
		}
		importedRaw = raw
		publicKeyHex = doc.PublicKeyHex
		// Verify password immediately so typos are caught before network choice.
		if _, err := keyhold.Unlock(doc, first); err != nil {
			fmt.Fprintln(os.Stderr, "km: 验证密码失败:", err)
			return exitError
		}
		privateKey = nil // new key: private only in KeyHold; unlock later via password
		_ = privateKey
	case "import", "i":
		path, err := promptLine("纯 KeyHold 文件路径: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 读取导入文件失败:", err)
			return exitError
		}
		doc, err := keyhold.Parse(raw)
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 导入文件不是有效的纯 KeyHold:", err)
			return exitError
		}
		password, err := readHiddenPassword("导入 KeyHold 解密密码: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		result, err := keyhold.Unlock(doc, password)
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 解密失败（密码错误或文件损坏）")
			return exitError
		}
		for i := range result.PrivateKey {
			result.PrivateKey[i] = 0
		}
		importedRaw = raw
		publicKeyHex = doc.PublicKeyHex
	default:
		fmt.Fprintln(os.Stderr, "km: 请输入 new 或 import")
		return exitUsage
	}
	// Other settings use defaults in this step; network is last.
	network, err := promptLine("选择网络 test | main（最后一步）: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	network = strings.ToLower(strings.TrimSpace(network))
	if network != "test" && network != "main" {
		fmt.Fprintln(os.Stderr, "km: 网络必须是 test 或 main")
		return exitError
	}
	// Full validation before persisting: KeyHold valid, pubkey known, network chosen.
	doc, err := keyhold.Parse(importedRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 内部 KeyHold 无效:", err)
		return exitError
	}
	if doc.PublicKeyHex != publicKeyHex {
		fmt.Fprintln(os.Stderr, "km: 钱包公钥不一致")
		return exitError
	}
	authHex := computeInitAuth(importedRaw, network, publicKeyHex)
	_ = authHex
	// Persist: info+settings first (recoverable marker), then key.json.
	// We need the private key to compute the HMAC binding; for a new key we
	// no longer have it in memory (only KeyHold). Re-derive via password?
	// For "new" we still have the password in scope? No (first/second out of
	// scope). Rework: retain password for binding computation.
	fmt.Fprintln(os.Stderr, "km: 初始化需要重新验证密码以生成网络绑定。")
	bindPassword, err := readHiddenPassword("再次输入钱包密码（生成网络绑定）: ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	unlocked, err := keyhold.Unlock(doc, bindPassword)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 密码错误，初始化取消（未写入半套数据）")
		return exitError
	}
	auth := computeHMAC(unlocked.PrivateKey, network, unlocked.PublicKeyHex)
	for i := range unlocked.PrivateKey {
		unlocked.PrivateKey[i] = 0
	}
	if unlocked.PublicKeyHex != publicKeyHex {
		fmt.Fprintln(os.Stderr, "km: 钱包公钥不一致")
		return exitError
	}
	store := appconfig.New(layout.Dir)
	if err := store.Init(network, publicKeyHex, auth); err != nil {
		fmt.Fprintln(os.Stderr, "km: 初始化失败:", err)
		return exitError
	}
	// Write pure KeyHold verbatim (pretty) last; HasInitMarkers requires all three.
	var pretty []byte
	{
		var v any
		if err := json.Unmarshal(importedRaw, &v); err != nil {
			_ = os.Remove(layout.InfoPath)
			_ = os.Remove(layout.SettingsPath)
			fmt.Fprintln(os.Stderr, "km: 导入文件无效")
			return exitError
		}
		pretty, err = json.MarshalIndent(v, "", "  ")
		if err != nil {
			_ = os.Remove(layout.InfoPath)
			_ = os.Remove(layout.SettingsPath)
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		pretty = append(pretty, '\n')
	}
	if err := write0600(layout.KeyPath, pretty); err != nil {
		_ = os.Remove(layout.InfoPath)
		_ = os.Remove(layout.SettingsPath)
		fmt.Fprintln(os.Stderr, "km: 写入钱包失败:", err)
		return exitError
	}
	fmt.Printf("已创建钱包: %s（网络 %s）\n", layout.Dir, network)
	return exitOK
}

func cmdDaemon(g globalFlags, args []string) int {
	noTray := false
	for _, a := range args {
		switch a {
		case "--no-tray", "--no-tray=true":
			noTray = true
		case "--no-tray=false":
			noTray = false
		default:
			fmt.Fprintln(os.Stderr, "用法: km daemon [--data-dir DIR] [--no-tray]")
			return exitUsage
		}
	}
	layout, err := session.ResolveLayout(g.dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	// Windows: when launched by SCM, run under the service host so Stop
	// maps to the same orderly shutdown with bounded cleanup.
	if handled, code := runAsWindowsServiceIfNeeded(layout.Dir); handled {
		return code
	}
	mode := daemonrun.TrayAuto
	if noTray {
		mode = daemonrun.TrayNever
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Ctrl-C keeps the historic exit code; all lifecycle rules
	// (tray isolation, serialized restart, close-and-wait) live in
	// daemonrun. Missing init, lock conflicts and startup failures
	// are reported there with the same messages as before.
	return daemonrun.Run(ctx, daemonrun.Options{
		DataDir:          layout.Dir,
		Tray:             mode,
		ExitCodeOnSignal: exitCancelled,
	})
}

// cmdInternalTray is the hidden tray child entry: same binary, no wallet,
// no keys, no config, no locks, no business APIs. Stdin/stdout carry only
// control frames; stderr carries diagnostics. It is never shown in usage.
func cmdInternalTray() int {
	// Refuse a terminal stdin: the child protocol needs a pipe from the
	// daemon parent; an interactive shell would block forever on input
	// with a stale tray showing.
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintln(os.Stderr, "km: internal-tray 是内部入口，不能直接运行（由 km daemon 按需启动）")
		return exitUsage
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	return tray.RunChild(os.Stdin, os.Stdout, logf)
}

func cmdUnlock(g globalFlags, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "km unlock 不接受参数")
		return exitUsage
	}
	// Unlock is for an external daemon. Never create an embedded daemon
	// that would exit immediately just to run unlock.
	base, err := session.ResolveBaseURL(g.api, g.dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		fmt.Fprintln(os.Stderr, "km: 本地命令（info/get）启动时会自行询问密码并解锁，无需单独 unlock")
		return exitError
	}
	if err := session.ValidateAPIURL(base); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	probe, err := session.DialExternal(context.Background(), base)
	if err == nil {
		defer probe.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var st kmapi.Status
	if err == nil {
		st, err = probe.Status(ctx)
	}
	if err != nil {
		if session.IsConnectionFailure(err) {
			fmt.Fprintln(os.Stderr, "km:", err)
			fmt.Fprintln(os.Stderr, "km: 没有外部后台；本地命令（info/get）启动时会自行询问密码，不创建随即退出的内嵌后台")
			return exitError
		}
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if err := kmapi.CheckIdentity(st); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	password, err := readHiddenPasswordWithContext(sigCtx, "钱包密码: ")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, "km: 已取消")
			return exitCancelled
		}
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if password == "" {
		fmt.Fprintln(os.Stderr, "km: 密码不能为空")
		return exitError
	}
	unlockCtx, unlockCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer unlockCancel()
	if err := probe.Unlock(unlockCtx, password); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	fmt.Println("已解锁")
	return exitOK
}

func cmdInfo(g globalFlags, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "km info 不接受参数")
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sess, warning, err := session.OpenSession(ctx, g.api, g.dataDir, readHiddenPasswordWithContext)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer sess.Close()
	if warning != "" {
		fmt.Fprintln(os.Stderr, "km: 提示:", warning)
	}
	info, err := sess.Client.Info(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	// Render: mode, network, lock, address, QR, balance with time/cache/error.
	network := info.Network
	locked := info.Locked
	address := info.Address
	if sess.Mode == "embedded" {
		fmt.Printf("模式: 内嵌后台（当前进程，退出即关闭）\n")
	} else {
		fmt.Printf("模式: 外部后台（CLI 退出后台继续）\n")
	}
	fmt.Printf("后台: %s\n", sess.APIURL)
	fmt.Printf("数据目录: %s\n", sess.DataDir)
	if network != "" {
		fmt.Printf("网络: %s\n", network)
	}
	if locked {
		fmt.Println("状态: 已锁定")
	} else {
		fmt.Println("状态: 已解锁")
	}
	if address != "" {
		fmt.Printf("收款地址: %s\n", address)
		printQR(address)
	}
	cached := info.Cached != nil && *info.Cached
	if info.Error != nil && *info.Error != "" {
		fmt.Printf("余额查询失败: %s\n", *info.Error)
		if cached {
			fmt.Println("（显示缓存，已标明时间，非实时）")
		}
	} else {
		if info.TotalSats != nil {
			fmt.Printf("余额: %d sats", *info.TotalSats)
			if info.AvailableSats != nil {
				fmt.Printf("（可用 %d", *info.AvailableSats)
				if info.AllocatedSats != nil {
					fmt.Printf("，占用 %d", *info.AllocatedSats)
				}
				fmt.Print("）")
			}
			fmt.Println()
		}
		if info.QueryTime != nil && *info.QueryTime != "" {
			if cached {
				fmt.Printf("查询时间: %s（缓存）\n", *info.QueryTime)
			} else {
				fmt.Printf("查询时间: %s\n", *info.QueryTime)
			}
		}
	}
	if !info.Connected {
		fmt.Fprintln(os.Stderr, "km: 后台未连接")
		return exitError
	}
	return exitOK
}

func cmdGet(g globalFlags, args []string) int {
	cancel := false
	rest := []string{}
	for _, a := range args {
		if a == "--cancel" {
			cancel = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 {
		fmt.Fprintln(os.Stderr, "用法: km get <seedhashhex> [--cancel]")
		return exitUsage
	}
	seed := strings.ToLower(strings.TrimSpace(rest[0]))
	if !isSeedHash(seed) {
		fmt.Fprintln(os.Stderr, "km: seedhashhex 无效（64 hex）")
		return exitUsage
	}
	if cancel {
		return cmdGetCancel(g, seed)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sess, warning, err := session.OpenSession(ctx, g.api, g.dataDir, readHiddenPasswordWithContext)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer sess.Close()
	if warning != "" {
		fmt.Fprintln(os.Stderr, "km: 提示:", warning)
	}
	client := sess.Client
	if sess.Mode == "embedded" {
		fmt.Fprintln(os.Stderr, "模式: 内嵌后台（Ctrl-C 将取消在途工作并有界收尾）")
	} else {
		fmt.Fprintln(os.Stderr, "模式: 外部后台（Ctrl-C 只停显示，后台继续）")
	}
	if _, err := client.CreateTask(ctx, seed); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if sess.Mode == "embedded" {
		fmt.Printf("已提交下载任务: %s（内嵌后台；完成并收尾后退出）\n", seed)
	} else {
		fmt.Printf("已提交下载任务: %s（后台落入默认下载根；断开只停显示，后台继续）\n", seed)
	}
	// Progress via the unified subscription (snapshot first, then changes).
	// Request cancellation never cancels the task itself; only explicit
	// cancel or (embedded) session close does.
	for {
		terminal, resync := watchTaskProgress(ctx, client, seed)
		if terminal != "" {
			switch terminal {
			case "done":
				// Session.Close waits for the execution goroutine and
				// index/refresh收尾, not just the state string.
				return exitOK
			case "failed":
				return exitError
			case "cancelled":
				return exitCancelled
			}
		}
		if resync {
			// Gap/overflow/instance change: query the original task once,
			// render it, then resubscribe. Never repurchase here.
			task, err := client.GetTask(ctx, seed)
			if err != nil {
				fmt.Fprintln(os.Stderr, "km:", err)
				fmt.Fprintln(os.Stderr, "km: 已提交任务，查询失败不重复采购；请重新 km get 查询原任务")
				return exitError
			}
			if st := renderTaskProgress(seed, task); st != "" {
				switch st {
				case "done":
					return exitOK
				case "failed":
					return exitError
				case "cancelled":
					return exitCancelled
				}
			}
			continue
		}
		if ctx.Err() != nil {
			if sess.Mode == "embedded" {
				fmt.Fprintln(os.Stderr, "已取消在途工作，有界收尾中…")
				// Session.Close (deferred) drains with bounds; report
				// cancelled, not success. The task returns to pending
				// for resume when stopped by shutdown.
				return exitCancelled
			}
			fmt.Fprintln(os.Stderr, "已断开显示，后台继续（重新 km get 可查询；取消用 km get --cancel）")
			return exitOK
		}
		// Subscribe ended (transport error or old backend): report like a
		// failed poll, never repurchase.
		fmt.Fprintln(os.Stderr, "km: 订阅中断")
		fmt.Fprintln(os.Stderr, "km: 已提交任务，查询失败不重复采购；请重新 km get 查询原任务")
		return exitError
	}
}

// watchTaskProgress streams one subscription until a terminal task state,
// a resync request, a transport failure, or command cancellation.
// Returns the terminal state ("" when none) and whether resync was requested.
// Cancellation is reported through ctx and handled by the caller, which
// keeps the embedded/external Ctrl-C distinction in one place.
func watchTaskProgress(ctx context.Context, client *kmapi.UnifiedClient, seed string) (terminal string, resync bool) {
	var done string
	err := client.SubscribeEvents(ctx, []string{kmapi.TopicTask},
		func(snap kmapi.SnapshotEvent) {
			tasks := snapshotTasks(snap, seed)
			for _, t := range tasks {
				if st := renderTaskProgress(seed, t); st != "" {
					done = st
				}
			}
		},
		func(env kmapi.EventEnvelope) bool {
			if env.Topic != kmapi.TopicTask {
				return true
			}
			if env.EntityID == nil || *env.EntityID != seed {
				return true
			}
			task, err := envelopeTask(env)
			if err != nil {
				return true
			}
			if st := renderTaskProgress(seed, task); st != "" {
				done = st
				return false
			}
			return true
		},
		func() bool {
			resync = true
			return false
		})
	if done != "" {
		return done, false
	}
	if resync {
		return "", true
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
	}
	return "", false
}

// snapshotTasks extracts our task from a snapshot (first frame, no loss).
func snapshotTasks(snap kmapi.SnapshotEvent, seed string) []kmapi.Task {
	if snap.State == nil {
		return nil
	}
	raw, ok := snap.State[kmapi.TopicTask]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var list kmapi.TaskList
	if err := json.Unmarshal(encoded, &list); err != nil {
		// Single-task shape fallback (forward tolerance).
		var one kmapi.Task
		if err := json.Unmarshal(encoded, &one); err != nil || one.SeedHashHex != seed {
			return nil
		}
		return []kmapi.Task{one}
	}
	var out []kmapi.Task
	for _, t := range list.Tasks {
		if t.SeedHashHex == seed {
			out = append(out, t)
		}
	}
	return out
}

// envelopeTask decodes a task change payload.
func envelopeTask(env kmapi.EventEnvelope) (kmapi.Task, error) {
	var task kmapi.Task
	if env.Data == nil {
		return task, fmt.Errorf("任务事件缺少数据")
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		return task, err
	}
	if err := json.Unmarshal(raw, &task); err != nil {
		return task, err
	}
	return task, nil
}

// renderTaskProgress prints one task state; returns the terminal state or "".
func renderTaskProgress(seed string, task kmapi.Task) string {
	state := task.State
	detail := ""
	if task.Detail != nil {
		detail = *task.Detail
	}
	fmt.Printf("任务 %s: %s %d/%d %s\n", seed, state, task.BlocksDone, task.BlocksTotal, detail)
	if len(task.Recommended) > 0 {
		fmt.Printf("推荐名: %s\n", strings.Join(task.Recommended, ", "))
	}
	switch state {
	case "done":
		if task.FinalPath != nil && *task.FinalPath != "" {
			fmt.Printf("完成: %s\n", *task.FinalPath)
		}
		return "done"
	case "failed":
		if task.Error != nil {
			fmt.Fprintln(os.Stderr, "km: 下载失败:", *task.Error)
		}
		return "failed"
	case "cancelled":
		fmt.Println("已取消")
		return "cancelled"
	}
	return ""
}

// cmdGetCancel only acts on an external daemon. Without one it never starts
// an embedded daemon just to cancel, never steals the data directory lock,
// and never adds an implicit cross-process cancel channel.
func cmdGetCancel(g globalFlags, seed string) int {
	base, err := session.ResolveBaseURL(g.api, g.dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		fmt.Fprintln(os.Stderr, "km: 没有外部后台时不抢占数据目录锁；本地执行者通过自身 Ctrl-C 取消")
		return exitError
	}
	if err := session.ValidateAPIURL(base); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	probe, err := session.DialExternal(context.Background(), base)
	if err == nil {
		defer probe.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var st kmapi.Status
	if err == nil {
		st, err = probe.Status(ctx)
	}
	if err != nil {
		if session.IsConnectionFailure(err) {
			// No daemon to cancel against. Distinguish lock-held from idle.
			layout, lerr := session.ResolveLayout(g.dataDir)
			if lerr == nil {
				if guardErr := session.TryLockHeld(layout.Dir); guardErr {
					fmt.Fprintln(os.Stderr, "km: 数据目录已有本地进程占用，不抢占，不新增跨进程取消通道；请用该进程的 Ctrl-C 取消")
				} else {
					fmt.Fprintln(os.Stderr, "km: 没有运行中的后台可取消（外部不可达，本地无占用）")
				}
			}
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if err := kmapi.CheckIdentity(st); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	if err := probe.CancelTask(cctx, seed); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	fmt.Println("已取消（停止新增采购，有界收尾；资金证据保留）")
	return exitOK
}
