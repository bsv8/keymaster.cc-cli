// Command km is the Keymaster CLI. This increment covers first-run
// initialization (network choice, KeyHold creation, config defaults), unlock
// with interactive or systemd credentials, identity display, signal handling
// with bounded exit codes. Download and server execution are wired in the next
// phases.
package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	primitives "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	channels "github.com/bsv8/ChannelProtocol"
	"github.com/bsv8/ChannelProtocol/hashrequest"
	"github.com/bsv8/bitcoin-libp2p/golibp2p"
	"github.com/bsv8/bitcoin-libp2p/signer"
	"github.com/bsv8/bitcoin-libp2p/webrtcinterconnect"
	"github.com/bsv8/go-bitfs/protocol"
	"github.com/bsv8/keymaster.cc-cli/internal/admin"
	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
	"github.com/bsv8/keymaster.cc-cli/internal/bitfs"
	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/config"
	"github.com/bsv8/keymaster.cc-cli/internal/discovery"
	"github.com/bsv8/keymaster.cc-cli/internal/download"
	"github.com/bsv8/keymaster.cc-cli/internal/ledger"
	"github.com/bsv8/keymaster.cc-cli/internal/output"
	"github.com/bsv8/keymaster.cc-cli/internal/pricing"
	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
	"github.com/bsv8/keymaster.cc-cli/internal/sell"
	websignal "github.com/bsv8/keymaster.cc-cli/internal/signal"
	"github.com/bsv8/keymaster.cc-cli/internal/ssp"
	"github.com/bsv8/keymaster.cc-cli/internal/state"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/transport"
	"github.com/bsv8/keymaster.cc-cli/internal/tui"
	"github.com/bsv8/keymaster.cc-cli/internal/ui"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	tpt "github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
	"golang.org/x/term"
)

// Exit codes are part of the CLI contract.
const (
	exitOK              = 0
	exitError           = 1
	exitUsage           = 2
	exitIncompleteFunds = 3
	exitCancelled       = 4
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	flags := flag.NewFlagSet("km", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	serverPath := flags.String("server", "", "售卖指定目录及子目录内的文件")
	discover := flags.Bool("discover", true, "售卖目录并接入供应商监听 Hash 需求（false 只扫描）")
	listen := flags.String("listen", "/ip4/0.0.0.0/tcp/0", "下载时向卖方公布的监听 multiaddr")
	adminAddr := flags.String("admin", "127.0.0.1:8790", "Web Admin 监听地址；空字符串禁用")
	outDir := flags.String("out", "", "下载输出目录（默认当前目录）")
	legacyDirect := flags.Bool("legacy-direct", false, "显式使用旧的单卖方直购路径（仅开发对照，禁止自动回退）")
	maxBlockPrice := flags.String("max-block-price", "", "买方价格上限（sats/块；空=使用 config.scheduler.maxBlockPriceSatoshis，0=自动）")
	configPath := flags.String("config", "", "配置文件路径（默认 ~/.km/config.json）")
	pendingConclusions := flags.Bool("pending-conclusions", false, "只读列出未决交易，不广播")
	queryConclusion := flags.String("query-conclusion", "", "显式启动指定 allocation 的原 exact 结论查询（会重复提交原交易）")
	queryInterval := flags.Duration("query-interval", 5*time.Second, "原 exact 查询重交间隔（至少1秒）")
	flags.Usage = func() { usage(os.Stderr) }
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	queryMode := *pendingConclusions || *queryConclusion != ""
	if queryMode && ((*pendingConclusions && *queryConclusion != "") || flags.NArg() != 0 || *serverPath != "" || *queryInterval < time.Second) {
		fmt.Fprintln(os.Stderr, "km: 只读未决列表与结论查询须独立调用，查询间隔至少1秒")
		return exitUsage
	}

	path := *configPath
	if path == "" {
		resolved, err := config.DefaultPath()
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		path = resolved
	}

	var (
		cfg         *config.Config
		keyDocument *wallet.NetworkKeyHold
		err         error
	)
	var password string
	if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
		if queryMode {
			fmt.Fprintln(os.Stderr, "km: 查询需要已有钱包配置")
			return exitError
		}
		cfg, password, err = initialize(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 初始化失败:", err)
			return exitError
		}
		keyDocument, err = config.LoadKey(config.KeyPath(path))
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 读取钱包失败:", err)
			return exitError
		}
	} else {
		if migrated, err := config.MigrateEmbeddedWallet(path); err != nil {
			fmt.Fprintln(os.Stderr, "km: 钱包迁移失败:", err)
			return exitError
		} else if migrated {
			fmt.Printf("已迁移钱包到 %s（原配置已备份）\n", config.KeyPath(path))
		}
		cfg, err = config.Load(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 读取配置失败:", err)
			return exitError
		}
		keyDocument, err = config.LoadKey(config.KeyPath(path))
		if err != nil {
			fmt.Fprintln(os.Stderr, "km: 读取钱包失败:", err)
			return exitError
		}
		if err := config.VerifyPair(cfg, keyDocument); err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		password, err = readPassword("钱包密码: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
	}
	if *maxBlockPrice != "" {
		cfg.Scheduler.MaxBlockPriceSatoshis = *maxBlockPrice
	}

	privateKey, publicKeyHex, err := keyDocument.Unlock(password)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer clearBytes(privateKey)

	address, err := addressFromPublicKey(publicKeyHex, cfg.BsvNetwork.Mainnet())
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 派生地址失败:", err)
		return exitError
	}
	if queryMode {
		fmt.Fprintf(os.Stderr, "网络: %s\n收款地址: %s\n", cfg.BsvNetwork, address)
	} else {
		fmt.Printf("网络: %s\n收款地址: %s\n", cfg.BsvNetwork, address)
	}

	// The signal context covers the execution phase; prompts above keep the
	// default terminal behaviour (with the password prompt restoring state).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if queryMode {
		return runConclusions(ctx, cfg, privateKey, path, *queryConclusion, *queryInterval)
	}
	return dispatch(ctx, cfg, privateKey, flags.NArg(), flags.Arg, *serverPath, *discover, *outDir, *listen, *adminAddr, path, *legacyDirect)
}

func runConclusions(ctx context.Context, cfg *config.Config, key []byte, path, allocationID string, interval time.Duration) int {
	w, err := wallet.New(woc.New(cfg.BsvNetwork.WOCBaseURL(), cfg.WOCAPIKey), wallet.Options{
		Network: string(cfg.BsvNetwork), PrivateKey: key, FeeRateSatoshisPerKb: cfg.FeeRateSatoshisPerKb,
		UnresolvedDir: config.ClientUnresolvedDir(path),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer w.Close()
	if allocationID == "" {
		pending, err := w.PendingConclusions()
		if err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		rows := []map[string]any{}
		for _, entry := range pending {
			rows = append(rows, map[string]any{"allocationId": entry.AllocationID, "commandId": entry.CommandID, "txid": entry.TxID, "state": entry.State, "delivery": entry.Delivery})
		}
		if err := json.NewEncoder(os.Stdout).Encode(rows); err != nil {
			fmt.Fprintln(os.Stderr, "km:", err)
			return exitError
		}
		return exitOK
	}
	fmt.Fprintln(os.Stderr, "km: 开始查询并重交原交易；Ctrl-C 停止，不释放未决资金")
	result, err := w.QueryConclusion(ctx, allocationID, interval)
	if result != nil {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		if ctx.Err() != nil {
			return exitCancelled
		}
		return exitError
	}
	if result != nil && result.Delivery == "failed" {
		return exitError
	}
	return exitOK
}

// dispatch runs the selected mode. Both modes are stubs until the execution
// layer lands; cancellation is still reported with the contract exit code.
func dispatch(ctx context.Context, cfg *config.Config, privateKey []byte, argCount int, arg func(int) string, serverPath string, discover bool, outDir, listen, adminAddr, configPath string, legacyDirect bool) int {
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "退出中/收尾中…")
		fmt.Fprintln(os.Stderr, "km: 已取消")
		return exitCancelled
	default:
	}

	code := 0
	switch {
	case serverPath != "":
		code = runServerScan(ctx, cfg, privateKey, serverPath, discover, adminAddr, configPath)
	case argCount == 1:
		seedHash := arg(0)
		resolvedOut := outDir
		if resolvedOut == "" {
			workingDirectory, err := os.Getwd()
			if err != nil {
				fmt.Fprintln(os.Stderr, "km:", err)
				return exitError
			}
			resolvedOut = workingDirectory
		}
		fmt.Printf("下载目标: %s\n输出目录: %s\n", seedHash, resolvedOut)
		code = runDownload(ctx, cfg, privateKey, seedHash, resolvedOut, listen, configPath, legacyDirect)
	default:
		usage(os.Stderr)
		return exitUsage
	}
	// Exit 3 marks money that is still unresolved, even if the command itself
	// succeeded; the records live in the client-level history.
	if code == exitOK {
		if records, err := state.List(filepath.Join(filepath.Dir(configPath), "client", "unresolved")); err == nil && len(records) > 0 {
			fmt.Fprintf(os.Stderr, "km: 存在 %d 条未决资金记录，退出码 3\n", len(records))
			return exitIncompleteFunds
		}
	}
	return code
}

// runServerScan 打开持久内容目录；Admin、发现和售卖共用同一个 Catalog。
func runServerScan(ctx context.Context, cfg *config.Config, privateKey []byte, directory string, discover bool, adminAddr, configPath string) int {
	files, err := catalog.Open(ctx, directory, filepath.Join(filepath.Dir(configPath), "server", "catalog.sqlite"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer files.Close()
	report, err := files.Refresh(ctx, false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	entries, err := files.List(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "退出中/收尾中…")
		return exitCancelled
	}
	fmt.Printf("索引目录: %s\n文件: %d 个，新计算: %d 个，复用: %d 个\n", files.Root, len(entries), report.Indexed, report.Reused)
	if len(report.Failures) > 0 {
		fmt.Printf("索引失败: %d 个（例如 %s: %s）\n", len(report.Failures), report.Failures[0].Path, report.Failures[0].Reason)
	}
	pricingPath := filepath.Join(filepath.Dir(configPath), "server", "pricing.json")
	table, err := pricing.Open(pricingPath, cfg.Server.DefaultSeedPriceSatoshis, cfg.Server.DefaultFullBlockPriceSatoshis)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.RelativePath)
	}
	if err := table.Prune(paths); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	seedPrice, blockPrice := table.Defaults()
	fmt.Printf("缺省售价: Seed %s sats / 完整块 %s sats；文件独立价格: %d 个\n", seedPrice, blockPrice, len(table.Files()))

	// The seller execution layer is ready; serving itself waits for the
	// demand/discovery and transport layers.
	key, _ := primitives.PrivateKeyFromBytes(privateKey)
	signer, err := protocol.NewPrivateKeySigner(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	arbiterKeys, err := cfg.RuntimeArbiterPublicKeys()
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	arbiters := make([]protocol.PublicKey, 0, len(arbiterKeys))
	for _, encoded := range arbiterKeys {
		raw, decodeErr := hex.DecodeString(encoded)
		if decodeErr != nil {
			fmt.Fprintf(os.Stderr, "km: 仲裁公钥 %s 不是 hex\n", encoded)
			return exitError
		}
		publicKey, keyErr := protocol.PublicKeyFromBytes(raw)
		if keyErr != nil {
			fmt.Fprintf(os.Stderr, "km: 仲裁公钥 %s 无效: %v\n", encoded, keyErr)
			return exitError
		}
		arbiters = append(arbiters, publicKey)
	}
	sessionsRoot := filepath.Join(filepath.Dir(configPath), "server", "sessions")
	sessions, err := store.Open(sessionsRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	wocClient := woc.New(cfg.BsvNetwork.WOCBaseURL(), cfg.WOCAPIKey)
	height := &chainHeight{}
	if fetched, err := wocClient.ChainHeight(ctx); err == nil {
		height.set(fetched)
	} else {
		fmt.Fprintln(os.Stderr, "km: 读取链高度失败（报价与卖方检查将不可用）:", err)
		return exitError
	}
	go height.refresh(ctx, wocClient)
	sellerWallet, err := wallet.New(wocClient, wallet.Options{
		Network:              string(cfg.BsvNetwork),
		PrivateKey:           privateKey,
		FeeRateSatoshisPerKb: cfg.FeeRateSatoshisPerKb,
		UnresolvedDir:        filepath.Join(filepath.Dir(configPath), "client", "unresolved"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer sellerWallet.Close()
	if recovered, pending, recoverErr := sellerWallet.Recover(ctx); recoverErr != nil {
		fmt.Fprintln(os.Stderr, "km: 卖方钱包恢复失败:", recoverErr)
		return exitError
	} else if len(pending) > 0 {
		fmt.Fprintf(os.Stderr, "km: 卖方钱包 %d 笔未知交易待确认（已恢复 %d 笔）\n", len(pending), recovered)
	}
	service := &sell.Seller{
		Sessions: sessions,
		Signer:   signer,
		Facts:    func() protocol.Facts { return protocol.Facts{Now: time.Now(), BlockHeight: height.get()} },
		Catalog:  files,
		Pricing:  table,
		Arbiters: arbiters,
		Wallet:   sellerWallet,
	}
	fmt.Printf("会话目录: %s\n仲裁方: %d 个\n", sessionsRoot, len(arbiters))
	// 周期核对和 SIGHUP 复用同一刷新入口，只重算新增或长度变化的文件。
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	refreshCtx, stopRefresh := context.WithCancel(ctx)
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-hup:
			case <-ticker.C:
			}
			report, err := files.Refresh(refreshCtx, false)
			if err != nil {
				if refreshCtx.Err() == nil {
					fmt.Fprintln(os.Stderr, "km: 目录核对失败:", err)
				}
				continue
			}
			if report.Indexed > 0 || report.Removed > 0 || len(report.Failures) > 0 {
				fmt.Printf("目录核对: 新计算 %d，复用 %d，移除 %d，失败 %d\n", report.Indexed, report.Reused, report.Removed, len(report.Failures))
			}
		}
	}()
	defer func() { stopRefresh(); <-refreshDone }()
	if len(arbiters) == 0 {
		fmt.Fprintln(os.Stderr, "km: 警告: 未配置 server.arbiterPublicKeys，买方无法开池（每个池是 2-of-3）。")
	}
	if adminAddr != "" {
		if cfg.Admin.APIKey == "" {
			fmt.Fprintln(os.Stderr, "km: admin.apiKey 为空，Web Admin 未启动")
		} else {
			adminServer, err := admin.Start(ctx, admin.Options{
				Address:       adminAddr,
				APIKey:        cfg.Admin.APIKey,
				Network:       string(cfg.BsvNetwork),
				WalletAddress: addressFromPublicKeyOrEmpty(cfg, privateKey),
				Catalog:       files,
				Pricing:       table,
				PersistDefaults: func(seedPrice, blockPrice string) error {
					cfg.Server.DefaultSeedPriceSatoshis = seedPrice
					cfg.Server.DefaultFullBlockPriceSatoshis = blockPrice
					return cfg.Save(configPath)
				},
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "km: Web Admin 启动失败:", err)
				return exitError
			}
			defer adminServer.Close()
			fmt.Printf("Web Admin: http://%s/（API Key 见 %s 的 admin.apiKey）\n", adminServer.Addr(), configPath)
		}
	}
	if !discover {
		fmt.Fprintln(os.Stderr, "km: 仅扫描模式（-discover=false）；未启动售卖。")
		return exitOK
	}
	return runDiscoverLoop(ctx, cfg, privateKey, service)
}

// runDiscoverLoop connects to the first enabled supplier, subscribes to the
// hash-request channel and reports verified demands until cancellation.
func runDiscoverLoop(ctx context.Context, cfg *config.Config, privateKey []byte, service *sell.Seller) int {
	services, err := cfg.RuntimeServices()
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	var supplier *config.Supplier
	for candidate := range services.SatSuppliers {
		if services.SatSuppliers[candidate].Enabled {
			supplier = &services.SatSuppliers[candidate]
			break
		}
	}
	if supplier == nil || len(supplier.Multiaddrs) == 0 {
		fmt.Fprintln(os.Stderr, "km: 启用 -discover 需要至少一个启用的 SatSubscription 供应商")
		return exitError
	}
	localSigner, err := signer.NewLocalSigner(privateKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 创建发现身份失败:", err)
		return exitError
	}
	client, err := ssp.Dial(ctx, ssp.Options{Gateway: supplier.Multiaddrs[0], Signer: localSigner})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 连接供应商失败:", err)
		return exitError
	}
	defer client.Close()
	channelKey, err := channels.ParsePrivateKey(privateKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	signaling, err := websignal.NewInbox(client, channelKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if err := signaling.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "km: 启动 WebRTC 信令失败:", err)
		return exitError
	}
	var webrtcTransport *webrtcinterconnect.Transport
	webrtcOptions := webrtcinterconnect.Options{Signaling: signaling}
	// Loopback ICE candidates are only useful for same-host tests and are
	// disabled in production. The e2e harness opts in explicitly.
	if os.Getenv("KM_WEBRTC_LOOPBACK_CANDIDATES") == "1" {
		webrtcOptions.IncludeLoopbackCandidates = true
	}
	baseFactory := webrtcinterconnect.Factory(webrtcOptions)
	factory := func(upgrader tpt.Upgrader, rcmgr network.ResourceManager, self peer.ID) (tpt.Transport, error) {
		transport, err := baseFactory(upgrader, rcmgr, self)
		if err != nil {
			return nil, err
		}
		webrtcTransport = transport.(*webrtcinterconnect.Transport)
		return transport, nil
	}
	sellerHost, err := golibp2p.NewHost(golibp2p.HostConfig{
		Signer:           localSigner,
		TransportOptions: []libp2p.Option{libp2p.Transport(factory)},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 创建传输身份失败:", err)
		return exitError
	}
	defer sellerHost.Close()
	publicKey := localSigner.PublicKey()
	sellerID := hex.EncodeToString(publicKey)
	admission := bitfs.OpenAdmission()
	handleDemand := func(demand discovery.Demand) {
		locator := ""
		for _, candidate := range demand.Locators {
			if candidate.Kind == hashrequest.LocatorMultiaddr && candidate.Address != "" {
				locator = candidate.Address
				break
			}
		}
		webrtcLocator := false
		for _, candidate := range demand.Locators {
			if candidate.Kind == hashrequest.LocatorWebRTCSDP {
				webrtcLocator = true
				break
			}
		}
		buyerKeyRaw, err := hex.DecodeString(demand.FromPublicKey)
		if err != nil {
			return
		}
		buyerKey, err := protocol.PublicKeyFromBytes(buyerKeyRaw)
		if err != nil {
			return
		}
		sessionID := "d-" + demand.MessageID
		resumable, err := service.FindResumableSession(demand.FromPublicKey, demand.SeedHashHex)
		if err != nil {
			fmt.Fprintf(os.Stderr, "km: 查找可续会话失败 %s: %v\n", demand.MessageID, err)
			return
		}
		if resumable != nil {
			sessionID = resumable.SessionID
			fmt.Printf("续接未完成会话 session=%s seed=%s\n", sessionID, demand.SeedHashHex)
		} else if _, err := service.CreateQuote(ctx, sell.QuoteRequest{
			SessionID:      sessionID,
			SellerID:       sellerID,
			BuyerPublicKey: buyerKey,
			SeedHashHex:    demand.SeedHashHex,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "km: 报价失败 %s: %v\n", demand.MessageID, err)
			return
		}
		if webrtcLocator {
			if webrtcTransport == nil {
				fmt.Fprintf(os.Stderr, "km: 需求 %s 需要 WebRTC 传输但未就绪\n", demand.MessageID)
				return
			}
			// The WebRTC dial waits for the buyer's SDP answer, which
			// arrives through the same SSP read loop that invoked this
			// callback: never block it.
			serveWebRTC(ctx, demand, sessionID, sellerID, service, signaling, webrtcTransport, sellerHost)
			return
		}
		if locator == "" {
			fmt.Fprintf(os.Stderr, "km: 需求 %s 没有可用 locator\n", demand.MessageID)
			return
		}
		session, err := transport.Dial(ctx, sellerHost, locator)
		if err != nil {
			fmt.Fprintf(os.Stderr, "km: 连接买方失败 %s: %v\n", demand.MessageID, err)
			return
		}
		fmt.Printf("已报价并连接买方 session=%s seed=%s\n", sessionID, demand.SeedHashHex)
		go func() { <-ctx.Done(); _ = session.Close() }()
		if err := service.Serve(ctx, sessionID, session); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "km: 售卖会话结束 session=%s: %v\n", sessionID, err)
		}
	}

	listener := &discovery.Listener{
		Subscribe: func(ctx context.Context, channel string) error {
			_, err := client.Subscribe(ctx, channel)
			return err
		},
		Deliveries: func(handler func(string, []byte)) { client.AddPublishHandler(handler) },
		OnDemand: func(demand discovery.Demand) {
			entry, err := service.Catalog.Preferred(ctx, demand.SeedHashHex)
			if err != nil {
				fmt.Fprintln(os.Stderr, "km: 查询供应内容失败:", err)
				return
			}
			if entry == nil {
				return
			}
			// Admission covers the whole unit, including connection setup:
			// either the sale is registered before shutdown closes admission,
			// or it is refused and never started.
			if !admission.Try() {
				return
			}
			go func() {
				defer admission.Done()
				handleDemand(demand)
			}()
		},
	}

	if err := listener.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "km: 订阅 Hash 需求失败:", err)
		return exitError
	}
	fmt.Printf("已接入供应商 %s（%s），监听 Hash 需求；Ctrl-C 结束。\n", supplier.Name, supplier.Multiaddrs[0])
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "退出中/收尾中…")
	// Stop new sessions (the watchers close their streams) and wait for the
	// in-flight seller sessions to reach a persisted step, bounded.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if err := admission.Wait(waitCtx); err != nil {
		fmt.Fprintln(os.Stderr, "km: 收尾超时，会话已持久化最近安全点")
	}
	return exitCancelled
}

// chainHeight caches the current chain height and refreshes it periodically.
type chainHeight struct {
	mu    sync.RWMutex
	value protocol.BlockHeight
}

func (c *chainHeight) set(value uint32) {
	c.mu.Lock()
	c.value = protocol.BlockHeight(value)
	c.mu.Unlock()
}

func (c *chainHeight) get() protocol.BlockHeight {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}

func (c *chainHeight) refresh(ctx context.Context, client *woc.Client) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if value, err := client.ChainHeight(ctx); err == nil {
				c.set(value)
			}
		}
	}
}

func usage(writer *os.File) {
	fmt.Fprintln(writer, `用法:
  km <seed-hash>                     下载一个文件（默认输出到当前目录）
  km -server <path>                  售卖指定目录（默认接入供应商并监听 Hash 需求）
  km -server <path> -discover=false  只扫描目录与定价，不启动售卖
  km -pending-conclusions           只读列出未决交易，不广播
  km -query-conclusion <allocation> 显式启动原交易结论查询（会重交同一 exact）

选项:
  -out <dir>              下载输出目录
  -listen <multiaddr>     下载时向卖方公布的监听地址（缺省 /ip4/0.0.0.0/tcp/0）
  -admin <addr>           Web Admin 监听地址（缺省 127.0.0.1:8790；空字符串禁用）
  -config <file>          配置文件路径（默认 ~/.km/config.json）
  -query-interval <duration>  结论查询间隔（默认5s，至少1s）

首次运行没有配置文件时，会在交互式终端中询问网络（main/test）并创建钱包。
非交互环境通过 systemd 凭据提供密码（CREDENTIALS_DIRECTORY/wallet-password）。`)
}

// initialize runs the first-run flow: network choice, password twice, key
// creation and network-scoped defaults written to config.
func initialize(path string) (*config.Config, string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, "", errors.New("没有配置文件时首次运行需要交互式终端；非交互环境请先在终端完成初始化")
	}
	network, err := promptNetwork()
	if err != nil {
		return nil, "", err
	}
	password, err := promptNewPassword()
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Init(path, network, password)
	if err != nil {
		return nil, "", err
	}
	fmt.Printf("已创建配置: %s\n", path)
	return cfg, password, nil
}

func promptNetwork() (config.Network, error) {
	reader := bufio.NewReader(os.Stdin)
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprint(os.Stderr, "选择网络 main/test: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		switch strings.TrimSpace(line) {
		case "main":
			return config.NetworkMain, nil
		case "test":
			return config.NetworkTest, nil
		default:
			fmt.Fprintln(os.Stderr, "请输入 main 或 test。")
		}
	}
	return "", errors.New("网络选择失败")
}

func promptNewPassword() (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		first, err := readPassword("设置钱包密码: ")
		if err != nil {
			return "", err
		}
		if first == "" {
			fmt.Fprintln(os.Stderr, "密码不能为空。")
			continue
		}
		second, err := readPassword("再次输入密码: ")
		if err != nil {
			return "", err
		}
		if first != second {
			fmt.Fprintln(os.Stderr, "两次密码不一致，请重试。")
			continue
		}
		return first, nil
	}
	return "", errors.New("密码设置失败")
}

// readPassword prefers the systemd credential directory; it never reads the
// password from command line arguments, environment variables or config.
// While the hidden prompt is active, SIGINT/SIGTERM restore the terminal state
// before exiting so the shell is not left without echo.
func readPassword(prompt string) (string, error) {
	if directory := os.Getenv("CREDENTIALS_DIRECTORY"); directory != "" {
		data, err := os.ReadFile(filepath.Join(directory, "wallet-password"))
		if err != nil {
			return "", fmt.Errorf("systemd 凭据 wallet-password 缺失: %w", err)
		}
		return strings.TrimRight(string(data), "\r\n"), nil
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("非交互环境需要 systemd 凭据 wallet-password")
	}
	terminalState, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("读取终端状态失败: %w", err)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		select {
		case <-signals:
			_ = term.Restore(fd, terminalState)
			fmt.Fprintln(os.Stderr, "\n已取消")
			os.Exit(exitCancelled)
		case <-done:
		}
	}()
	fmt.Fprint(os.Stderr, prompt)
	value, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(value), "\r\n"), nil
}

func addressFromPublicKey(publicKeyHex string, mainnet bool) (string, error) {
	publicKey, err := primitives.PublicKeyFromString(publicKeyHex)
	if err != nil {
		return "", err
	}
	address, err := script.NewAddressFromPublicKey(publicKey, mainnet)
	if err != nil {
		return "", err
	}
	return address.AddressString, nil
}

func clearBytes(buffer []byte) {
	for index := range buffer {
		buffer[index] = 0
	}
}

// runDownload publishes one Hash demand, waits for a seller session over the
// BitFS transport and purchases the file.
func runDownload(ctx context.Context, cfg *config.Config, privateKey []byte, seedHash, outputDir, listen, configPath string, legacyDirect bool) int {
	clientRoot := filepath.Join(filepath.Dir(configPath), "client", seedHash)
	if err := os.MkdirAll(clientRoot, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	wocClient := woc.New(cfg.BsvNetwork.WOCBaseURL(), cfg.WOCAPIKey)
	height, err := wocClient.ChainHeight(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 读取链高度失败:", err)
		return exitError
	}
	w, err := wallet.New(wocClient, wallet.Options{
		Network:              string(cfg.BsvNetwork),
		PrivateKey:           privateKey,
		FeeRateSatoshisPerKb: cfg.FeeRateSatoshisPerKb,
		UnresolvedDir:        filepath.Join(filepath.Dir(configPath), "client", "unresolved"),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	defer w.Close()
	if recovered, pending, recoverErr := w.Recover(ctx); recoverErr != nil {
		fmt.Fprintln(os.Stderr, "km: 钱包恢复失败:", recoverErr)
		return exitError
	} else if len(pending) > 0 {
		fmt.Fprintf(os.Stderr, "km: 钱包 %d 笔未知交易待确认（已恢复 %d 笔）\n", len(pending), recovered)
	}
	workStore, err := store.Open(clientRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	l, err := ledger.Open(filepath.Join(clientRoot, "ledger.json"), seedHash, string(cfg.BsvNetwork), seedHash)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	if legacyDirect {
		// The direct path must never run after the scheduler produced pool,
		// authorization or payment evidence: switching would buy again.
		hasSnapshot, _ := workStore.Exists("scheduler/tasks/" + seedHash + ".json")
		if hasSnapshot || len(l.Pools()) > 0 {
			fmt.Fprintln(os.Stderr, "km: 存在调度器任务或在途资金证据，禁止切换到旧直购路径")
			return exitError
		}
	}
	key, _ := primitives.PrivateKeyFromBytes(privateKey)
	signerInstance, err := protocol.NewPrivateKeySigner(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	facts := func() protocol.Facts {
		return protocol.Facts{Now: time.Now(), BlockHeight: protocol.BlockHeight(height)}
	}
	task := &bitfs.BuyerTask{
		Wallet: w, Ledger: l, Store: workStore, Signer: signerInstance,
		Facts: facts, MinerFeeRate: protocol.SatoshisPerKilobyte(cfg.FeeRateSatoshisPerKb),
		Chain: wocClient, UnresolvedDir: config.ClientUnresolvedDir(configPath),
	}
	localSigner, err := signer.NewLocalSigner(privateKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	listenAddr, err := ma.NewMultiaddr(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 监听地址无效:", err)
		return exitError
	}
	services, err := cfg.RuntimeServices()
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	buyerHost, err := golibp2p.NewHost(golibp2p.HostConfig{
		Signer:           localSigner,
		ListenAddrs:      []ma.Multiaddr{listenAddr},
		TransportOptions: []libp2p.Option{libp2p.Transport(websocket.New)},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 创建监听失败:", err)
		return exitError
	}
	defer buyerHost.Close()
	addresses := buyerHost.Addrs()
	if len(addresses) == 0 {
		fmt.Fprintln(os.Stderr, "km: 没有可用监听地址")
		return exitError
	}
	locator, err := hashrequest.NewMultiaddrLocator(addresses[0].Encapsulate(ma.StringCast("/p2p/" + buyerHost.ID().String())).String())
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	arbitrationClient := &arbitration.Client{Host: buyerHost, Endpoints: services.ArbitrationServices}
	var supplier *config.Supplier
	for index := range services.SatSuppliers {
		if services.SatSuppliers[index].Enabled {
			supplier = &services.SatSuppliers[index]
			break
		}
	}
	if supplier == nil || len(supplier.Multiaddrs) == 0 {
		fmt.Fprintln(os.Stderr, "km: 下载需要至少一个启用的 SatSubscription 供应商")
		return exitError
	}
	client, err := ssp.Dial(ctx, ssp.Options{Gateway: supplier.Multiaddrs[0], Signer: localSigner})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 连接供应商失败:", err)
		return exitError
	}
	defer client.Close()
	channelKey, err := channels.ParsePrivateKey(privateKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	channelPublic, err := channels.PublicKeyFromPrivate(channelKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return exitError
	}
	publisher := &discovery.Publisher{
		Publish: func(ctx context.Context, channel string, contentJSON []byte) error {
			_, err := client.Publish(ctx, channel, contentJSON)
			return err
		},
		PrivateKey: channelKey,
		PublicKey:  channelPublic,
		Locators:   []hashrequest.Locator{locator},
	}
	var result *bitfs.PurchaseResult
	var purchaseErr error
	renderer := ui.New(os.Stdout, term.IsTerminal(int(os.Stdout.Fd())), true)
	renderer.Render(ui.State{Stage: ui.StageUnlock, Address: addressFromPublicKeyOrEmpty(cfg, privateKey), SeedHash: seedHash})
	renderer.Render(ui.State{Stage: ui.StageDiscovery, SeedHash: seedHash, Detail: "供应商: " + supplier.Name})
	poolID := seedHash + "-pool"
	resume := false
	if assembler, err := output.New(workStore, outputDir); err == nil {
		if final, err := assembler.Final(); err == nil && final != nil {
			renderer.Render(ui.State{Stage: ui.StageDone, SeedHash: seedHash, Path: final.Path, FileSize: uint64(final.Bytes)})
			return exitOK
		}
	}
	if poolRecord, ok := l.Pools()[poolID]; ok && poolRecord.State != ledger.PoolClosed {
		if _, err := bitfs.LoadPoolState(workStore, poolID); err == nil {
			resume = true
			renderer.Render(ui.State{Stage: ui.StageQuote, SeedHash: seedHash, Detail: "续接上次未完成的采购"})
		}
	}
	sessionCh := make(chan *transport.Session, 8)
	var (
		claimed  bitfs.Claim
		doneOnce sync.Once
		activeMu sync.Mutex
		active   *transport.Session
	)
	done := make(chan struct{})
	finish := func() { doneOnce.Do(func() { close(done) }) }
	var listenErr error
	if legacyDirect {
		listenErr = transport.Listen(buyerHost, func(session *transport.Session) {
			// One direct purchase owns one pool and one ledger: a second
			// concurrent seller session must never run over the same state.
			if !claimed.Try() {
				_ = session.Close()
				return
			}
			activeMu.Lock()
			active = session
			activeMu.Unlock()
			defer finish()
			defer session.Close()
			renderer.Render(ui.State{Stage: ui.StageWaiting, SeedHash: seedHash, Detail: locator.Address})
			result, purchaseErr = task.RunPurchase(ctx, session, bitfs.PurchaseOptions{
				Arbitration:       arbitrationClient,
				PoolID:            poolID,
				SeedHashHex:       seedHash,
				OutputDir:         outputDir,
				RefundDelayBlocks: 5,
				Resume:            resume,
				Progress: func(progress bitfs.PurchaseProgress) {
					renderer.Render(ui.State{
						Stage:      ui.Stage(progress.Stage),
						SeedHash:   seedHash,
						Detail:     progress.Detail,
						BlocksDone: progress.BlocksDone,
						Blocks:     progress.BlocksTotal,
					})
				},
			})
		})
	} else {
		listenErr = transport.Listen(buyerHost, func(session *transport.Session) {
			select {
			case sessionCh <- session:
			default:
				_ = session.Close()
			}
		})
	}
	if listenErr != nil {
		fmt.Fprintln(os.Stderr, "km: 监听 BitFS 会话失败:", listenErr)
		return exitError
	}
	if !legacyDirect {
		lastViewRender := time.Now()
		var viewMu sync.Mutex
		var latestView scheduler.View
		var recMu sync.Mutex
		var recommendations []string
		runCtx, runCancel := context.WithCancel(ctx)
		defer runCancel()
		renderer.Render(ui.State{Stage: ui.StageWaiting, SeedHash: seedHash, Detail: "调度器下载（" + locator.Address + "）"})
		result, purchaseErr = download.Run(runCtx, download.Options{
			Arbitration: arbitrationClient,
			Task:        task, SeedHash: seedHash, OutputDir: outputDir,
			Wallet: w, Store: workStore,
			Accept: func(acceptCtx context.Context) (*transport.Session, error) {
				if _, err := publisher.PublishDemand(acceptCtx, seedHash, 10*time.Minute); err != nil {
					return nil, err
				}
				select {
				case session := <-sessionCh:
					return session, nil
				case <-acceptCtx.Done():
					return nil, acceptCtx.Err()
				}
			},
			Logf: func(format string, args ...any) { fmt.Fprintf(os.Stderr, "km: "+format+"\n", args...) },
			OnView: func(view scheduler.View) {
				viewMu.Lock()
				latestView = view
				viewMu.Unlock()
				if time.Since(lastViewRender) < time.Second {
					return
				}
				lastViewRender = time.Now()
				recMu.Lock()
				recommended := append([]string(nil), recommendations...)
				recMu.Unlock()
				detail := "调度器: " + view.Execution + "，已完成 " + fmt.Sprint(view.Completed) + "/" + fmt.Sprint(view.Total)
				if len(recommended) > 0 {
					detail += "，推荐: " + strings.Join(recommended, ", ")
				}
				state := ui.State{
					Stage: ui.StageWaiting, SeedHash: seedHash,
					Address:    addressFromPublicKeyOrEmpty(cfg, privateKey),
					Detail:     detail,
					BlocksDone: view.Completed, Blocks: view.Total,
					PriceMode: view.Price.Mode, PriceEffective: view.Price.Effective,
				}
				if view.Price.Range != nil {
					state.PriceMin = view.Price.Range.Min
					state.PriceMax = view.Price.Range.Max
				}
				renderer.Render(state)
			},
			OnRecommendations: func(names []string) {
				recMu.Lock()
				recommendations = append([]string(nil), names...)
				recMu.Unlock()
			},
			Ready: func(feed func(scheduler.Event) error) func() {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return func() {}
				}
				keyCtx, keyCancel := context.WithCancel(runCtx)
				done := make(chan struct{})
				go func() {
					defer close(done)
					tui.RunPriceKeys(keyCtx, feed, func() scheduler.View {
						viewMu.Lock()
						defer viewMu.Unlock()
						return latestView
					}, runCancel)
				}()
				return func() {
					keyCancel()
					<-done
				}
			},
			Config: scheduler.Config{
				MaxBlockPrice:    cfg.Scheduler.MaxBlockPriceSatoshis,
				BaseSeats:        int64(cfg.Scheduler.TotalSeats - cfg.Scheduler.FloatingSeats),
				FloatingSeats:    int64(cfg.Scheduler.FloatingSeats),
				PoolBlocks:       int64(cfg.Scheduler.PoolBlocks),
				RequestTimeoutMs: int64(max(1, cfg.Scheduler.RequestTimeoutMs)),
			},
		})
		if runCtx.Err() != nil || ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "退出中/收尾中…")
			return exitCancelled
		}
		if purchaseErr != nil {
			renderer.Render(ui.State{Stage: ui.StageFailed, SeedHash: seedHash, Error: purchaseErr.Error()})
			fmt.Fprintln(os.Stderr, "km: 下载失败:", purchaseErr)
			return exitError
		}
		if len(result.RecommendedNames) > 0 {
			fmt.Printf("推荐名: %s\n", strings.Join(result.RecommendedNames, ", "))
		}
		if len(result.RecommendedNames) > 1 && term.IsTerminal(int(os.Stdin.Fd())) {
			fmt.Println("选择最终文件名:")
			for index, name := range result.RecommendedNames {
				fmt.Printf("  %d. %s\n", index+1, name)
			}
			fmt.Printf("输入 [1-%d]，回车使用 1: ", len(result.RecommendedNames))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			choice := 1
			if value := strings.TrimSpace(line); value != "" {
				if parsed, err := strconv.Atoi(value); err == nil && parsed >= 1 && parsed <= len(result.RecommendedNames) {
					choice = parsed
				}
			}
			if assembler, err := output.New(workStore, outputDir); err == nil {
				if renamed, err := assembler.RenameFinal(result.RecommendedNames[choice-1]); err == nil {
					result.Path = renamed.Path
				}
			}
		}
		renderer.Render(ui.State{Stage: ui.StageDone, SeedHash: seedHash, Path: result.Path, FileSize: uint64(result.Bytes), Paid: result.Committed})
		return exitOK
	}
	// Register the inbound handler before publishing the demand: a seller can
	// react immediately and must never find the stream handler missing.
	if _, err := publisher.PublishDemand(ctx, seedHash, 10*time.Minute); err != nil {
		fmt.Fprintln(os.Stderr, "km: 发布需求失败:", err)
		return exitError
	}
	fmt.Printf("已发布需求，等待卖方连接（%s）…\n", locator.Address)
	select {
	case <-done:
	case <-ctx.Done():
		// Bounded shutdown: stop new work, unblock the in-flight session so the
		// purchase can persist its last safe point, then exit.
		fmt.Fprintln(os.Stderr, "退出中/收尾中…")
		activeMu.Lock()
		if active != nil {
			_ = active.Close()
		}
		activeMu.Unlock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			fmt.Fprintln(os.Stderr, "km: 收尾超时，已持久化最近安全点")
		}
		return exitCancelled
	}
	if purchaseErr != nil {
		renderer.Render(ui.State{Stage: ui.StageFailed, SeedHash: seedHash, Error: purchaseErr.Error()})
		fmt.Fprintln(os.Stderr, "km: 下载失败:", purchaseErr)
		return exitError
	}
	renderer.Render(ui.State{Stage: ui.StageDone, SeedHash: seedHash, Path: result.Path, FileSize: uint64(result.Bytes), Paid: result.Committed})
	return exitOK
}

// serveWebRTC dials a browser buyer over WebRTC: the signal exchange goes
// through the buyer's inbox and the interconnect connection uses the
// ChannelProtocol session id, exactly like the browser expects.
func serveWebRTC(ctx context.Context, demand discovery.Demand, sessionID, sellerID string, service *sell.Seller, signaling *websignal.Inbox, webrtcTransport *webrtcinterconnect.Transport, sellerHost host.Host) {
	// The BitFS session id names the seller session on disk; the interconnect
	// connection id must be a ChannelProtocol WebRTC session id (32-byte
	// base64url), which the browser also uses as its routing key.
	webrtcSessionID, err := websignal.NewSessionID()
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: 生成 WebRTC 会话编号失败 %s: %v\n", demand.MessageID, err)
		return
	}
	attemptID := webrtcSessionID
	requestMessageID, err := channels.ParseMessageID(demand.MessageID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: 需求编号无效 %s: %v\n", demand.MessageID, err)
		return
	}
	buyerPublic, err := channels.ParsePublicKey(demand.FromPublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: 买方公钥无效 %s: %v\n", demand.MessageID, err)
		return
	}
	if err := signaling.OpenSession(webrtcSessionID, attemptID, requestMessageID, buyerPublic); err != nil {
		fmt.Fprintf(os.Stderr, "km: 打开信令会话失败 %s: %v\n", demand.MessageID, err)
		return
	}
	buyerPeer, err := websignal.BuyerPeerID(demand.FromPublicKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: 派生买方 PeerId 失败 %s: %v\n", demand.MessageID, err)
		return
	}
	peerID, err := peer.Decode(buyerPeer)
	if err != nil {
		fmt.Fprintf(os.Stderr, "km: 买方 PeerId 无效 %s: %v\n", demand.MessageID, err)
		return
	}
	if err := webrtcTransport.QueueSessionIDs(webrtcSessionID, attemptID); err != nil {
		fmt.Fprintf(os.Stderr, "km: 绑定 WebRTC 会话失败: %v\n", err)
		return
	}
	address := webrtcinterconnect.BuildAddress(peerID)
	if err := sellerHost.Connect(ctx, peer.AddrInfo{ID: peerID, Addrs: []ma.Multiaddr{address}}); err != nil {
		signaling.CloseSession(webrtcSessionID)
		fmt.Fprintf(os.Stderr, "km: WebRTC 连接买方失败 %s: %v\n", demand.MessageID, err)
		return
	}
	stream, err := sellerHost.NewStream(ctx, peerID, transport.ProtocolID)
	if err != nil {
		signaling.CloseSession(webrtcSessionID)
		fmt.Fprintf(os.Stderr, "km: 打开 BitFS 流失败 %s: %v\n", demand.MessageID, err)
		return
	}
	session, err := transport.NewSession(stream)
	if err != nil {
		signaling.CloseSession(webrtcSessionID)
		fmt.Fprintf(os.Stderr, "km: 包装 BitFS 流失败 %s: %v\n", demand.MessageID, err)
		return
	}
	fmt.Printf("已报价并经 WebRTC 连接买方 session=%s seed=%s\n", sessionID, demand.SeedHashHex)
	go func() { <-ctx.Done(); _ = session.Close() }()
	defer signaling.CloseSession(webrtcSessionID)
	if err := service.Serve(ctx, sessionID, session); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "km: 售卖会话结束 session=%s: %v\n", sessionID, err)
	}
}

// addressFromPublicKeyOrEmpty derives the wallet address for display.
func addressFromPublicKeyOrEmpty(cfg *config.Config, privateKey []byte) string {
	key, _ := primitives.PrivateKeyFromBytes(privateKey)
	address, err := script.NewAddressFromPublicKey(key.PubKey(), cfg.BsvNetwork.Mainnet())
	if err != nil {
		return ""
	}
	return address.AddressString
}
