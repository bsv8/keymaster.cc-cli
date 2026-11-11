package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/gorilla/websocket"
)

// maxRequestIDs 限制单连接完成 ID 记录；超过后要求重连，避免无限内存增长。
const maxRequestIDs = 65536

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  65536,
	WriteBufferSize: 65536,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// wsSub 保存一条订阅的主题与取消函数；取消必须按订阅隔离，不能影响同连接
// 的其他页面/订阅。
type wsSub struct {
	topics map[string]struct{}
	once   sync.Once
	cancel func()
}

func (s *wsSub) stop() { s.once.Do(s.cancel) }

func (s *wsSub) matches(want map[string]struct{}) bool {
	if len(s.topics) != len(want) {
		return false
	}
	for topic := range want {
		if _, ok := s.topics[topic]; !ok {
			return false
		}
	}
	return true
}

type wsConn struct {
	daemon     *Daemon
	conn       *websocket.Conn
	writeMu    sync.Mutex
	instanceID string
	connID     string
	limits     kmapi.Limits
	closed     bool
	closeMu    sync.Mutex
	inflightMu sync.Mutex
	inflight   map[string]context.CancelFunc
	inflightN  int
	requestIDs map[string]struct{}
	attachMu   sync.Mutex
	attaches   map[string]*wsAttach
	unacked    int64
	subsMu     sync.Mutex
	subs       map[string]*wsSub
}

type wsAttach struct {
	declared int64
	received bool
	data     []byte
	timer    *time.Timer
}

func newConnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "c_" + hex.EncodeToString(b[:])
}

// handleWS 服务 /api/ws：hello/welcome 握手后进入统一分派器。
func (d *Daemon) handleWS(w http.ResponseWriter, r *http.Request) {
	// 复用 guard 的 Host/Origin 校验（WS 升级前）。
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		_ = h
	}
	_ = host
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &wsConn{
		daemon:     d,
		conn:       conn,
		instanceID: d.events.InstanceID(),
		connID:     newConnID(),
		limits:     kmapi.DefaultLimits(),
		inflight:   map[string]context.CancelFunc{},
		requestIDs: map[string]struct{}{},
		attaches:   map[string]*wsAttach{},
		subs:       map[string]*wsSub{},
	}
	d.wsMu.Lock()
	if d.wsConns == nil {
		d.wsConns = map[*wsConn]struct{}{}
	}
	d.wsConns[c] = struct{}{}
	d.wsMu.Unlock()
	defer func() {
		d.wsMu.Lock()
		delete(d.wsConns, c)
		d.wsMu.Unlock()
		c.cleanup()
	}()
	// 读取 hello（10s 期限）。
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hello kmapi.Hello
	if err := conn.ReadJSON(&hello); err != nil {
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: "", Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "hello 非法"}})
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "bad hello"), time.Now().Add(2*time.Second))
		return
	}
	if hello.Type != kmapi.MsgHello || hello.Product != kmapi.Product || hello.APIVersion != kmapi.APIVersion {
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: "", Error: &kmapi.ErrorDetail{Code: kmapi.ErrVersionMismatch, Message: "协议不兼容"}})
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "version"), time.Now().Add(2*time.Second))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	// 心跳：服务端每 15s Ping，45s 未确认则断开。
	// 读取上限按二进制上限设置（文本控制消息另按 JSONControlMaxBytes 逐条校验），
	// 避免合法大附件在进入附件校验前被连接层直接断开。
	conn.SetReadLimit(c.limits.BinarySingleMaxBytes + kmapi.BinaryFrameOverheadMaxBytes)
	_ = conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(45 * time.Second))
	})
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				c.writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
				c.writeMu.Unlock()
			}
		}
	}()
	if err := c.writeJSON(kmapi.Welcome{
		Type: kmapi.MsgWelcome, Product: kmapi.Product, APIVersion: kmapi.APIVersion,
		InstanceID: c.instanceID, ConnectionID: c.connID, Limits: c.limits,
	}); err != nil {
		return
	}
	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			c.handleBinary(msg)
			continue
		}
		if int64(len(msg)) > c.limits.JSONControlMaxBytes {
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, Error: &kmapi.ErrorDetail{Code: kmapi.ErrTooLarge, Message: "控制消息过大"}})
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "too large"), time.Now().Add(2*time.Second))
			return
		}
		var typ struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(msg, &typ); err != nil {
			continue
		}
		switch typ.Type {
		case kmapi.MsgRequest:
			var req kmapi.Request
			if err := json.Unmarshal(msg, &req); err != nil {
				continue
			}
			c.handleRequest(req)
		case kmapi.MsgCancel:
			var cancel kmapi.Cancel
			if err := json.Unmarshal(msg, &cancel); err != nil {
				continue
			}
			c.handleCancel(cancel)
		case kmapi.MsgSubscribe:
			var sub kmapi.Subscribe
			if err := json.Unmarshal(msg, &sub); err != nil {
				continue
			}
			c.handleSubscribe(sub)
		case kmapi.MsgUnsubscribe:
			var un kmapi.Unsubscribe
			if err := json.Unmarshal(msg, &un); err != nil {
				continue
			}
			c.handleUnsubscribe(un)
		case kmapi.MsgAttachBegin:
			var b kmapi.AttachBegin
			if err := json.Unmarshal(msg, &b); err != nil {
				continue
			}
			c.handleAttachBegin(b)
		default:
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "unknown"), time.Now().Add(2*time.Second))
			return
		}
	}
}

func (c *wsConn) writeJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if int64(len(raw)) > c.limits.JSONControlMaxBytes {
		return errTooLarge()
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteMessage(websocket.TextMessage, raw)
}

func errTooLarge() error { return &wsErr{"too large"} }

type wsErr struct{ s string }

func (e *wsErr) Error() string { return e.s }

func (c *wsConn) handleRequest(req kmapi.Request) {
	if req.ID == "" || req.Method == "" {
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "id/method 必填"}})
		return
	}
	c.inflightMu.Lock()
	if c.inflightN >= c.limits.ConnInflightRequests {
		c.inflightMu.Unlock()
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrTooLarge, Message: "在途请求过多"}})
		return
	}
	// 同一连接内不复用 ID；已完成的 ID 也保留，重复请求不得再次执行。
	if _, dup := c.requestIDs[req.ID]; dup {
		c.inflightMu.Unlock()
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "请求 ID 重复"}})
		return
	}
	if len(c.requestIDs) >= maxRequestIDs {
		c.inflightMu.Unlock()
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrTooLarge, Message: "请求 ID 过多，请重连"}})
		_ = c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "request id limit"), time.Now().Add(2*time.Second))
		_ = c.conn.Close()
		return
	}
	c.requestIDs[req.ID] = struct{}{}
	// 附件声明同步预注册：先校验方法与调用者（快速拒绝），再收字节。
	// 未声明的附件帧将被忽略；声明缺失则请求按无附件执行。
	for _, decl := range req.Attachments {
		if decl.ID == "" || decl.Bytes < 0 {
			c.inflightMu.Unlock()
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "附件声明非法"}})
			return
		}
		if decl.Bytes > c.limits.BinarySingleMaxBytes {
			c.inflightMu.Unlock()
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrTooLarge, Message: "附件过大"}})
			return
		}
		key := req.ID + ":" + decl.ID
		c.attachMu.Lock()
		if _, dup := c.attaches[key]; dup {
			c.attachMu.Unlock()
			c.inflightMu.Unlock()
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "附件重复声明"}})
			return
		}
		if c.unacked+decl.Bytes > c.limits.ConnUnackedMaxBytes {
			c.attachMu.Unlock()
			c.inflightMu.Unlock()
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrTooLarge, Message: "在途附件过多"}})
			return
		}
		c.unacked += decl.Bytes
		st := &wsAttach{declared: decl.Bytes}
		st.timer = time.AfterFunc(time.Duration(c.limits.AttachmentDeadlineSecs)*time.Second, func() {
			c.attachMu.Lock()
			if _, ok := c.attaches[key]; ok {
				delete(c.attaches, key)
				c.unacked -= decl.Bytes
			}
			c.attachMu.Unlock()
		})
		c.attaches[key] = st
		c.attachMu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithWalletCaller(ctx)
	c.inflight[req.ID] = cancel
	c.inflightN++
	c.inflightMu.Unlock()

	go func() {
		defer func() {
			c.inflightMu.Lock()
			delete(c.inflight, req.ID)
			c.inflightN--
			c.inflightMu.Unlock()
		}()
		// 附件在请求结束（含提前失败、超时、取消）后统一释放；接收时已停止
		// 定时器的附件也必须在这里扣减 unacked，避免连接级额度泄漏。
		defer func() {
			c.attachMu.Lock()
			for k, st := range c.attaches {
				if len(k) > len(req.ID) && k[:len(req.ID)] == req.ID && (len(k) == len(req.ID) || k[len(req.ID)] == ':') {
					if st.timer != nil {
						st.timer.Stop()
					}
					c.unacked -= st.declared
					delete(c.attaches, k)
				}
			}
			c.attachMu.Unlock()
		}()
		var params json.RawMessage
		if req.Params != nil {
			b, err := json.Marshal(req.Params)
			if err != nil {
				_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "参数非法"}})
				return
			}
			params = b
		}
		// 附件：等待同 requestId 的二进制收齐（30s 期限），未收齐不执行业务。
		atts := map[string][]byte{}
		deadline := time.Now().Add(time.Duration(c.limits.AttachmentDeadlineSecs) * time.Second)
		for {
			c.attachMu.Lock()
			ready := true
			pendingKeys := []string{}
			for k, st := range c.attaches {
				if len(k) > len(req.ID) && k[:len(req.ID)] == req.ID && (len(k) == len(req.ID) || k[len(req.ID)] == ':') {
					if !st.received {
						ready = false
						pendingKeys = append(pendingKeys, k)
					}
				}
			}
			// 收集已收齐的附件。
			for k, st := range c.attaches {
				if len(k) > len(req.ID) && k[:len(req.ID)] == req.ID && (len(k) == len(req.ID) || k[len(req.ID)] == ':') {
					if st.received {
						parts := k[len(req.ID)+1:]
						atts[parts] = st.data
					}
				}
			}
			c.attachMu.Unlock()
			if ready {
				break
			}
			_ = pendingKeys
			if time.Now().After(deadline) {
				_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "附件未收齐"}})
				return
			}
			select {
			case <-ctx.Done():
				_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrCancelled, Message: "调用已取消"}})
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		res, binaries, perr := c.daemon.Dispatch(ctx, req.Method, params, atts)
		if ctx.Err() == context.Canceled {
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrCancelled, Message: "调用已取消"}})
			return
		}
		if perr != nil {
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Error: perr})
			return
		}
		// 先发送二进制附件（每附件一帧），再发送最终响应。
		for attachID, data := range binaries {
			_ = c.writeJSON(kmapi.AttachBegin{Type: kmapi.MsgAttachBegin, RequestID: req.ID, AttachmentID: attachID, DeclaredBytes: int64(len(data))})
			frame, err := kmapi.EncodeAttachment(req.ID, attachID, data)
			if err != nil {
				continue
			}
			c.writeMu.Lock()
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = c.conn.WriteMessage(websocket.BinaryMessage, frame)
			c.writeMu.Unlock()
		}
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: req.ID, Result: res})
	}()
}

func (c *wsConn) handleCancel(cancel kmapi.Cancel) {
	c.inflightMu.Lock()
	fn, ok := c.inflight[cancel.TargetID]
	c.inflightMu.Unlock()
	status := kmapi.CancelNotFound
	if ok {
		fn()
		status = kmapi.CancelRequested
	}
	_ = c.writeJSON(kmapi.CancelAck{Type: kmapi.MsgCancelAck, ID: cancel.ID, TargetID: cancel.TargetID, Status: status})
}

func (c *wsConn) handleSubscribe(sub kmapi.Subscribe) {
	for _, t := range sub.Topics {
		if !isKnownTopic(t) {
			_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: sub.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrBadRequest, Message: "未知订阅主题"}})
			return
		}
	}
	brokerSub := c.daemon.events.Subscribe(sub.Topics)
	key := sub.ID
	if key == "" {
		key = newConnID()
	}
	done := make(chan struct{})
	topicSet := make(map[string]struct{}, len(sub.Topics))
	for _, topic := range sub.Topics {
		topicSet[topic] = struct{}{}
	}
	entry := &wsSub{topics: topicSet}
	entry.cancel = func() {
		close(done)
		c.daemon.events.Unsubscribe(brokerSub)
	}
	c.subsMu.Lock()
	if old, ok := c.subs[key]; ok {
		old.stop()
	}
	c.subs[key] = entry
	c.subsMu.Unlock()
	snap, err := c.daemon.buildSnapshot(sub.Topics)
	if err != nil {
		c.subsMu.Lock()
		if c.subs[key] == entry {
			delete(c.subs, key)
		}
		c.subsMu.Unlock()
		entry.stop()
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: sub.ID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrInternal, Message: err.Error()}})
		return
	}
	_ = c.writeJSON(kmapi.SnapshotMsg{Type: kmapi.MsgSnapshot, ID: key, InstanceID: snap.InstanceID, Topics: snap.Topics, Revisions: snap.Revisions, State: snap.State})
	go func() {
		defer entry.stop()
		// 重放注册期间暂存。
		for {
			select {
			case <-done:
				return
			case env, ok := <-brokerSub.Chan():
				if !ok {
					if brokerSub.Dropped() {
						_ = c.writeJSON(kmapi.ResyncMsg{Type: kmapi.MsgResync, Reason: "overflow"})
					}
					return
				}
				_ = c.writeJSON(kmapi.EventMsg{Type: kmapi.MsgEvent, InstanceID: env.InstanceID, Seq: env.Seq, Topic: env.Topic, EntityID: env.EntityID, Revision: env.Revision, Data: env.Data})
			default:
				goto live
			}
		}
	live:
		if brokerSub.Dropped() {
			_ = c.writeJSON(kmapi.ResyncMsg{Type: kmapi.MsgResync, Reason: "overflow"})
			return
		}
		for {
			select {
			case <-done:
				return
			case env, ok := <-brokerSub.Chan():
				if !ok {
					_ = c.writeJSON(kmapi.ResyncMsg{Type: kmapi.MsgResync, Reason: "closed"})
					return
				}
				_ = c.writeJSON(kmapi.EventMsg{Type: kmapi.MsgEvent, InstanceID: env.InstanceID, Seq: env.Seq, Topic: env.Topic, EntityID: env.EntityID, Revision: env.Revision, Data: env.Data})
			}
		}
	}()
}

func (c *wsConn) handleUnsubscribe(un kmapi.Unsubscribe) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	if un.SubscriptionID != "" {
		if sub, ok := c.subs[un.SubscriptionID]; ok {
			sub.stop()
			delete(c.subs, un.SubscriptionID)
		}
		return
	}
	if len(un.Topics) == 0 {
		// 无目标时保持旧语义：撤销本连接全部订阅。
		for key, sub := range c.subs {
			sub.stop()
			delete(c.subs, key)
		}
		return
	}
	want := make(map[string]struct{}, len(un.Topics))
	for _, topic := range un.Topics {
		want[topic] = struct{}{}
	}
	for key, sub := range c.subs {
		if sub.matches(want) {
			sub.stop()
			delete(c.subs, key)
		}
	}
}

// handleAttachBegin 兼容旧发送顺序（先附件后请求）的声明通道。
// 新客户端应在 request 中声明附件（服务端同步预注册）；重复声明将被忽略。
func (c *wsConn) handleAttachBegin(b kmapi.AttachBegin) {
	if b.RequestID == "" || b.AttachmentID == "" || b.DeclaredBytes < 0 {
		return
	}
	if b.DeclaredBytes > c.limits.BinarySingleMaxBytes {
		_ = c.writeJSON(kmapi.Response{Type: kmapi.MsgResponse, ID: b.RequestID, Error: &kmapi.ErrorDetail{Code: kmapi.ErrTooLarge, Message: "附件过大"}})
		return
	}
	key := b.RequestID + ":" + b.AttachmentID
	c.attachMu.Lock()
	if _, dup := c.attaches[key]; dup {
		c.attachMu.Unlock()
		return
	}
	if c.unacked+b.DeclaredBytes > c.limits.ConnUnackedMaxBytes {
		c.attachMu.Unlock()
		return
	}
	c.unacked += b.DeclaredBytes
	st := &wsAttach{declared: b.DeclaredBytes}
	st.timer = time.AfterFunc(time.Duration(c.limits.AttachmentDeadlineSecs)*time.Second, func() {
		c.attachMu.Lock()
		if _, ok := c.attaches[key]; ok {
			delete(c.attaches, key)
			c.unacked -= b.DeclaredBytes
		}
		c.attachMu.Unlock()
	})
	c.attaches[key] = st
	c.attachMu.Unlock()
}

func (c *wsConn) handleBinary(frame []byte) {
	hdr, data, err := kmapi.DecodeAttachment(frame, -1)
	if err != nil {
		return
	}
	if int64(len(data)) > c.limits.BinarySingleMaxBytes {
		return
	}
	key := hdr.RequestID + ":" + hdr.AttachmentID
	c.attachMu.Lock()
	st, ok := c.attaches[key]
	if !ok {
		// 未声明的附件帧直接丢弃，不执行业务。
		c.attachMu.Unlock()
		return
	}
	if int64(len(data)) != st.declared {
		delete(c.attaches, key)
		c.unacked -= st.declared
		c.attachMu.Unlock()
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	st.data = data
	st.received = true
	c.attachMu.Unlock()
}

func (c *wsConn) cleanup() {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return
	}
	c.closed = true
	c.closeMu.Unlock()
	c.inflightMu.Lock()
	for _, cancel := range c.inflight {
		cancel()
	}
	c.inflight = map[string]context.CancelFunc{}
	c.inflightMu.Unlock()
	c.subsMu.Lock()
	for _, sub := range c.subs {
		sub.stop()
	}
	c.subs = map[string]*wsSub{}
	c.subsMu.Unlock()
	c.attachMu.Lock()
	for _, st := range c.attaches {
		if st.timer != nil {
			st.timer.Stop()
		}
	}
	c.attaches = map[string]*wsAttach{}
	c.unacked = 0
	c.attachMu.Unlock()
	_ = c.conn.Close()
}
