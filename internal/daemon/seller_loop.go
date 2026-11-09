package daemon

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"

	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
	"github.com/bsv8/BitFSProtocol/protocol"
	channels "github.com/bsv8/ChannelProtocol"
	"github.com/bsv8/ChannelProtocol/hashrequest"
	"github.com/bsv8/bitcoin-libp2p/golibp2p"
	"github.com/bsv8/bitcoin-libp2p/signer"
	"github.com/bsv8/bitcoin-libp2p/webrtcinterconnect"
	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	tpt "github.com/libp2p/go-libp2p/core/transport"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"

	primitives "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
	"github.com/bsv8/keymaster.cc-cli/internal/bitfs"
	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/discovery"
	"github.com/bsv8/keymaster.cc-cli/internal/pricing"
	"github.com/bsv8/keymaster.cc-cli/internal/sell"
	websignal "github.com/bsv8/keymaster.cc-cli/internal/signal"
	"github.com/bsv8/keymaster.cc-cli/internal/ssp"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-cli/internal/transport"
	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

// sellerRuntime tracks the background seller loop started after unlock.
type sellerRuntime struct {
	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func (d *Daemon) ensureSellerStarted() {
	if d.isStopping() {
		return
	}
	d.sellerMu.Lock()
	if d.sellerRunning {
		d.sellerMu.Unlock()
		return
	}
	d.sellerRunning = true
	d.sellerMu.Unlock()
	// Prefer the daemon lifecycle as parent so Close always drains the
	// seller; fall back to Background for daemons that never called Start
	// (e.g. older embedded uses in tests).
	parent := d.LifecycleContext()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	d.mu.Lock()
	// Close may have started while we were preparing: do not publish a new
	// cancel after stopping began.
	if d.state == StateStopping {
		d.mu.Unlock()
		cancel()
		d.sellerMu.Lock()
		d.sellerRunning = false
		d.sellerMu.Unlock()
		return
	}
	d.sellerCancel = cancel
	d.mu.Unlock()
	d.sellerWg.Add(1)
	go func() {
		defer d.sellerWg.Done()
		d.runSellerLoop(ctx)
	}()
}

// runSellerLoop serves all roots until the daemon stops. New demands always
// quote from the root that actually holds the content; sessions keep their
// root binding and never switch to another root's same-named file.
func (d *Daemon) runSellerLoop(ctx context.Context) {
	defer func() {
		d.sellerMu.Lock()
		d.sellerRunning = false
		d.sellerMu.Unlock()
	}()
	info, settings, err := d.store.Snapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 售卖循环加载配置失败:", err)
		return
	}
	d.mu.Lock()
	privateKey := append([]byte(nil), d.privateKey...)
	d.mu.Unlock()
	if len(privateKey) == 0 {
		return
	}
	defer clearBytes(privateKey)
	wocClient := woc.New(wocBaseURL(info.BsvNetwork), settings.WOCAPIKey)
	initialHeight, err := wocClient.ChainHeight(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 读取链高度失败（报价与卖方检查将不可用）:", err)
		return
	}
	// Shared tip: quotes and protocol checks must not run on a launch-time
	// value while the loop lives for days.
	tip := &chainHeight{}
	tip.set(initialHeight)
	go tip.refreshLoop(ctx, wocClient)
	sellerWallet, releaseWallet, err := wallet.Shared(wocClient, wallet.Options{
		Network: info.BsvNetwork, PrivateKey: privateKey,
		FeeRateSatoshisPerKb: settings.FeeRateSatoshisPerKb,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 售卖钱包失败:", err)
		return
	}
	defer releaseWallet()
	key, _ := primitives.PrivateKeyFromBytes(privateKey)
	signerInstance, err := protocol.NewPrivateKeySigner(key)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return
	}
	services, err := loadRuntimeServices(info, settings)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return
	}
	// Arbiters always include the runtime directory identities (built-in
	// defaults, custom and dev overrides), plus explicitly accepted keys.
	seenArbiter := map[string]bool{}
	var arbiterKeys []string
	for _, endpoint := range services.ArbitrationServices {
		if endpoint.PublicKeyHex == "" || seenArbiter[endpoint.PublicKeyHex] {
			continue
		}
		seenArbiter[endpoint.PublicKeyHex] = true
		arbiterKeys = append(arbiterKeys, endpoint.PublicKeyHex)
	}
	for _, key := range settings.Server.ArbiterPublicKeys {
		if key == "" || seenArbiter[key] {
			continue
		}
		seenArbiter[key] = true
		arbiterKeys = append(arbiterKeys, key)
	}
	arbiters := make([]protocol.PublicKey, 0, len(arbiterKeys))
	for _, encoded := range arbiterKeys {
		raw, err := hex.DecodeString(encoded)
		if err != nil {
			continue
		}
		publicKey, err := protocol.PublicKeyFromBytes(raw)
		if err != nil {
			continue
		}
		arbiters = append(arbiters, publicKey)
	}
	sessions, err := store.Open(d.layout.SessionsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return
	}
	// Multi-root lookup helper.
	findRoot := func(seedHash string) (*catalog.Catalog, *pricing.Pricing, string, *catalog.Entry) {
		for _, h := range d.roots.list() {
			if h.deleted {
				continue
			}
			if h.catalog == nil || h.pricing == nil {
				continue
			}
			entry, err := h.catalog.Preferred(ctx, seedHash)
			if err != nil || entry == nil {
				continue
			}
			return h.catalog, h.pricing, h.root.ID, entry
		}
		return nil, nil, "", nil
	}
	_ = findRoot
	localSigner, err := signer.NewLocalSigner(privateKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 创建发现身份失败:", err)
		return
	}
	sspClient, err := ssp.Dial(ctx, ssp.Options{Gateway: firstSupplier(services), Signer: localSigner})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 连接供应商失败:", err)
		return
	}
	defer sspClient.Close()
	channelKey, err := channels.ParsePrivateKey(privateKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return
	}
	signaling, err := websignal.NewInbox(sspClient, channelKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "km:", err)
		return
	}
	if err := signaling.Start(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "km: 启动 WebRTC 信令失败:", err)
		return
	}
	var webrtcTransport *webrtcinterconnect.Transport
	webrtcOptions := webrtcinterconnect.Options{Signaling: signaling}
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
		TransportOptions: []libp2p.Option{libp2p.Transport(factory), libp2p.Transport(websocket.New)},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "km: 创建传输身份失败:", err)
		return
	}
	defer sellerHost.Close()
	arbitrationClient := &arbitration.Client{Host: sellerHost, Endpoints: services.ArbitrationServices}
	publicKey := localSigner.PublicKey()
	sellerID := hex.EncodeToString(publicKey)
	admission := bitfs.OpenAdmission()
	handleDemand := func(demand discovery.Demand) {
		// Find the root that holds the content.
		var chosenCat *catalog.Catalog
		var chosenPricing *pricing.Pricing
		var chosenRootID string
		for _, h := range d.roots.list() {
			if h.deleted {
				continue
			}
			if h.catalog == nil || h.pricing == nil {
				continue
			}
			entry, err := h.catalog.Preferred(ctx, demand.SeedHashHex)
			if err != nil || entry == nil {
				continue
			}
			chosenCat, chosenPricing, chosenRootID = h.catalog, h.pricing, h.root.ID
			break
		}
		if chosenCat == nil {
			return
		}
		service := &sell.Seller{
			Sessions: sessions, Signer: signerInstance,
			Facts: func() protocol.Facts {
				return protocol.Facts{Now: time.Now(), BlockHeight: protocol.BlockHeight(tip.get())}
			},
			Catalog: chosenCat, Pricing: chosenPricing, RootID: chosenRootID,
			Arbiters: arbiters, Wallet: sellerWallet, Arbitration: arbitrationClient,
		}
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
		if _, err := service.CreateQuote(ctx, sell.QuoteRequest{
			SessionID: sessionID, SellerID: sellerID,
			BuyerPublicKey: buyerKey, SeedHashHex: demand.SeedHashHex,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "km: 报价失败 %s: %v\n", demand.MessageID, err)
			return
		}
		if webrtcLocator {
			if webrtcTransport == nil {
				fmt.Fprintf(os.Stderr, "km: 需求 %s 需要 WebRTC 传输但未就绪\n", demand.MessageID)
				return
			}
			serveWebRTCDemand(ctx, d.roots, demand, sessionID, service, signaling, webrtcTransport, sellerHost)
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
		fmt.Printf("已报价并连接买方 session=%s seed=%s root=%s\n", sessionID, demand.SeedHashHex, chosenRootID)
		go func() { <-ctx.Done(); _ = session.Close() }()
		d.roots.retain(chosenRootID)
		serveErr := service.Serve(ctx, sessionID, session)
		d.roots.release(chosenRootID)
		if serveErr != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "km: 售卖会话结束 session=%s: %v\n", sessionID, serveErr)
		}
	}
	listener := &discovery.Listener{
		Subscribe: func(ctx context.Context, channel string) error {
			_, err := sspClient.Subscribe(ctx, channel)
			return err
		},
		Deliveries: func(handler func(string, []byte)) { sspClient.AddPublishHandler(handler) },
		OnDemand: func(demand discovery.Demand) {
			bitfsdebug.Log("keymaster.cc-cli", "demand.received", func() any { return map[string]any{"messageId": demand.MessageID, "seedHashHex": demand.SeedHashHex} })
			// Check all roots for the content.
			found := false
			for _, h := range d.roots.list() {
				if h.deleted {
					continue
				}
				if h.catalog == nil {
					continue
				}
				entry, err := h.catalog.Preferred(ctx, demand.SeedHashHex)
				if err != nil {
					continue
				}
				if entry != nil {
					found = true
					break
				}
			}
			if !found {
				return
			}
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
		return
	}
	fmt.Println("已接入供应商，监听 Hash 需求（多根）。")
	<-ctx.Done()
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := admission.Wait(waitCtx); err != nil {
		fmt.Fprintln(os.Stderr, "km: 收尾超时，会话已持久化最近安全点")
	}
}

func firstSupplier(services runtimeServices) string {
	for _, s := range services.SatSuppliers {
		if s.Enabled && len(s.Multiaddrs) > 0 {
			return s.Multiaddrs[0]
		}
	}
	return ""
}

func serveWebRTCDemand(ctx context.Context, roots *rootsManager, demand discovery.Demand, sessionID string, service *sell.Seller, signaling *websignal.Inbox, webrtcTransport *webrtcinterconnect.Transport, sellerHost host.Host) {
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
	roots.retain(service.RootID)
	serveErr := service.Serve(ctx, sessionID, session)
	roots.release(service.RootID)
	if serveErr != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "km: 售卖会话结束 session=%s: %v\n", sessionID, serveErr)
	}
}
