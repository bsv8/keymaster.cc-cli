// Package session 是 CLI 与未来 GUI 共用的前端会话层：统一处理后台连接、
// 三级回退、密码输入、后台所有权与清理。命令或视图只负责交互，不直接
// 操作后台业务对象。
//
// 新会话依次尝试：① 显式 --api → ② 本机 settings.json 端口对应地址 →
// ③ 独占启动内嵌后台。仅连接失败时尝试下一级；业务错误、钱包锁定、
// 协议不兼容、配置损坏、URL 非法明确报错，不绕过错误选择另一钱包。
//
// 外部后台走 WebSocket（/api/ws），内嵌后台走无端口进程内通信，
// 共用同一分派器。握手返回产品、版本与实例标识；版本不兼容不回退。
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/daemon"
	"github.com/bsv8/keymaster.cc-cli/internal/datadir"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
)

// PasswordReader 按提示读取密码。CLI 用终端隐藏输入，GUI 用原生密码界面。
type PasswordReader func(ctx context.Context, prompt string) (string, error)

// Session 统一外部与内嵌后台访问。命令只使用 Session.Client；Session 负责清理。
type Session struct {
	Client  *kmapi.UnifiedClient
	Mode    string // "external" or "embedded"
	DataDir string
	APIURL  string
	Daemon  *daemon.Daemon
}

// Close 释放会话资源。内嵌模式停止自持后台（有界收尾、清密钥、释放锁）；
// 外部模式只关闭连接，不关闭共享后台。
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if s.Client != nil {
		_ = s.Client.Close()
	}
	if s.Mode == "embedded" && s.Daemon != nil {
		return s.Daemon.Close()
	}
	return nil
}

// Candidate 是一级回退候选。
type Candidate struct {
	// Source 是候选来源："显式 --api" 或 "本机配置"。
	Source string
	URL    string
}

// ResolveLayout 解析数据目录布局。
func ResolveLayout(dataDir string) (datadir.Layout, error) {
	return datadir.Resolve(dataDir)
}

// TryLockHeld 报告是否有其他进程持有数据目录锁（探测后立即释放，不抢占）。
func TryLockHeld(dir string) bool {
	guard, err := dlock.Acquire(dir)
	if err != nil {
		return errors.Is(err, dlock.ErrActive)
	}
	_ = guard.Release()
	return false
}

// ValidateAPIURL 在探测前拒绝不支持的协议与畸形地址，使其明确报错而不
// 回退到本地钱包。支持 http/https 基地址与显式 ws/wss 地址。
func ValidateAPIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("API 地址非法: %w", err)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return fmt.Errorf("API 地址协议不支持 %q，只支持 http/https/ws/wss", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("API 地址缺少主机: %q", raw)
	}
	return nil
}

// IsConnectionFailure 报告是否为连接失败（拒绝、超时、DNS），这类失败才
// 允许尝试下一级。业务错误、协议不兼容与调用者取消永不回退。
func IsConnectionFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var terr *kmapi.TransportError
	if errors.As(err, &terr) {
		if errors.Is(terr.Err, context.Canceled) {
			return false
		}
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

// ResolveBaseURL 保持单地址解析（显式优先，否则本机配置），供 unlock、
// cancel、service status 等“只连外部、不回退内嵌”的命令使用。
// 需要完整三级回退的命令请用 OpenSession。
func ResolveBaseURL(explicitAPI, dataDir string) (string, error) {
	cands, err := candidates(explicitAPI, dataDir)
	if err != nil {
		return "", err
	}
	if len(cands) == 0 {
		return "", fmt.Errorf("无本机配置且未提供 --api，请提供 --api 或先运行 km init/daemon")
	}
	return cands[0].URL, nil
}

// normalize 去空格并裁尾斜杠，用于候选去重。
func normalize(raw string) string {
	return strings.TrimSuffix(strings.TrimSpace(raw), "/")
}

// settingsAddr 按需读取本机 settings.json 端口。文件不存在返回 ""（跳过
// 本级）；损坏返回 explicit 错误（不回退、不掩盖）。
func settingsAddr(dataDir string) (string, error) {
	dir := dataDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("请提供 --api（无法定位数据目录）: %w", err)
		}
		dir = filepath.Join(home, ".km")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("无本机配置且未提供 --api，请提供 --api 或先运行 km init/daemon: %w", err)
	}
	var settings struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("读取本机配置失败: %w", err)
	}
	if settings.Port == 0 {
		settings.Port = 2100
	}
	return fmt.Sprintf("http://127.0.0.1:%d", settings.Port), nil
}

// candidates 按优先级构造去重后的候选：显式 --api → 本机配置地址。
// 显式地址非法直接报错；本机配置损坏直接报错；都没有则返回空列表。
func candidates(explicitAPI, dataDir string) ([]Candidate, error) {
	var out []Candidate
	seen := map[string]bool{}
	add := func(source, u string) {
		n := normalize(u)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, Candidate{Source: source, URL: n})
	}
	if strings.TrimSpace(explicitAPI) != "" {
		n := normalize(explicitAPI)
		if err := ValidateAPIURL(n); err != nil {
			return nil, err
		}
		add("显式 --api", n)
	}
	addr, err := settingsAddr(dataDir)
	if err != nil {
		if strings.TrimSpace(explicitAPI) == "" {
			return nil, err
		}
		return out, nil
	}
	if addr != "" {
		add("本机配置", addr)
	}
	return out, nil
}

func DialExternal(ctx context.Context, rawURL string) (*kmapi.UnifiedClient, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ws, err := kmapi.DialWS(dialCtx, rawURL, kmapi.ClientInfo{Name: "km-cli", Version: "v1"})
	if err != nil {
		return nil, err
	}
	u := kmapi.NewUnifiedWS(ws)
	// 握手后业务探测：版本不兼容直接返回，不换后台。
	st, err := u.Status(ctx)
	if err != nil {
		_ = ws.Close()
		return nil, err
	}
	if err := kmapi.CheckIdentity(st); err != nil {
		_ = ws.Close()
		return nil, err
	}
	return u, nil
}

// OpenSession 按三级回退建立会话：显式 --api → 本机配置地址 → 内嵌后台。
//
//   - 候选按需读取；远程已连接时不要求本地钱包存在。
//   - 仅连接失败尝试下一级，并记录每一级失败原因；业务错误、锁定、
//     非法地址、协议不兼容、调用者取消直接返回。
//   - 前两级均不可连时，检查本地初始化状态再竞争独占锁；未初始化引导
//     km init，不自动建钱包；锁被占用明确报错，不抢锁、不起第二个实例。
//   - 返回的 warning 说明回退过程与最终选择；显式远端不可达后落到本地
//     时必须讲清“当前操作的是本地钱包，非远程”。
func OpenSession(ctx context.Context, explicitAPI, dataDir string, readPassword PasswordReader) (*Session, string, error) {
	layout, err := ResolveLayout(dataDir)
	if err != nil {
		return nil, "", err
	}

	cands, err := candidates(explicitAPI, layout.Dir)
	if err != nil {
		return nil, "", err
	}
	var failures []string
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		client, perr := DialExternal(ctx, c.URL)
		if perr == nil {
			return &Session{
				Client:  client,
				Mode:    "external",
				DataDir: layout.Dir,
				APIURL:  c.URL,
			}, fallbackWarning(explicitAPI, c, failures), nil
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if !IsConnectionFailure(perr) {
			return nil, "", perr
		}
		failures = append(failures, fmt.Sprintf("%s %s 连接失败（%v）", c.Source, c.URL, perr))
	}

	sess, err := openEmbedded(ctx, layout.Dir, readPassword)
	if err != nil {
		if len(failures) > 0 {
			return nil, "", fmt.Errorf("%s；%w", strings.Join(failures, "；"), err)
		}
		return nil, "", err
	}
	warning := ""
	if len(failures) > 0 {
		warning = strings.Join(failures, "；") + "，改用本地内嵌后台"
		if extra := describeLocal(sess); extra != "" {
			warning += "；" + extra
		}
		if strings.TrimSpace(explicitAPI) != "" {
			warning += "（显式 --api 未连接，当前操作的是本地钱包，非远程）"
		}
	}
	return sess, warning, nil
}

// fallbackWarning 描述回退结果：直连首选候选成功时返回 ""；显式地址
// 失败后落到其他后台时讲清当前操作的不是远端钱包。
func fallbackWarning(explicitAPI string, used Candidate, failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	warning := strings.Join(failures, "；") + "，改用" + used.Source + " " + used.URL
	if strings.TrimSpace(explicitAPI) != "" {
		warning += "（显式 --api 未连接，当前操作的是本地钱包，非远程）"
	}
	return warning
}

func openEmbeddedOrInitHint(ctx context.Context, dir string, readPassword PasswordReader) (*Session, string, error) {
	layout, err := ResolveLayout(dir)
	if err != nil {
		return nil, "", err
	}
	if !layout.HasInitMarkers() {
		if layout.HasAnyWallet() {
			return nil, "", fmt.Errorf("数据不完整或为旧版，请先运行 km init（迁移或修复）")
		}
		return nil, "", fmt.Errorf("尚未初始化，请先运行 km init")
	}
	return openEmbeddedWithWarning(ctx, layout.Dir, readPassword)
}

func openEmbedded(ctx context.Context, dir string, readPassword PasswordReader) (*Session, error) {
	sess, _, err := openEmbeddedWithWarning(ctx, dir, readPassword)
	return sess, err
}

func openEmbeddedWithWarning(ctx context.Context, dir string, readPassword PasswordReader) (*Session, string, error) {
	layout, err := ResolveLayout(dir)
	if err != nil {
		return nil, "", err
	}
	if !layout.HasInitMarkers() {
		if layout.HasAnyWallet() {
			return nil, "", fmt.Errorf("数据不完整或为旧版，请先运行 km init（迁移或修复）")
		}
		return nil, "", fmt.Errorf("尚未初始化，请先运行 km init")
	}
	d, err := daemon.Open(daemon.Options{DataDir: layout.Dir})
	if err != nil {
		if errors.Is(err, dlock.ErrActive) {
			return nil, "", fmt.Errorf("数据目录已有后台在运行，不启动第二个钱包实例")
		}
		return nil, "", err
	}
	success := false
	defer func() {
		if !success {
			_ = d.Close()
		}
	}()
	if err := d.Start(ctx); err != nil {
		return nil, "", err
	}
	if readPassword == nil {
		return nil, "", fmt.Errorf("内嵌后台需要密码，但未提供密码输入")
	}
	password, err := readPassword(ctx, "钱包密码: ")
	if err != nil {
		return nil, "", err
	}
	if password == "" {
		return nil, "", fmt.Errorf("密码不能为空")
	}
	if err := d.Unlock(password); err != nil {
		return nil, "", err
	}
	// 进程内调用同一分派器，不经过网络。
	inproc := kmapi.NewInprocWithEvents(d, d, d.InstanceID())
	client := kmapi.NewUnifiedInproc(inproc)
	if st, err := client.Status(ctx); err != nil {
		return nil, "", fmt.Errorf("内嵌后台自检失败: %w", err)
	} else if err := kmapi.CheckIdentity(st); err != nil {
		return nil, "", err
	}
	success = true
	return &Session{
		Client:  client,
		Mode:    "embedded",
		DataDir: layout.Dir,
		APIURL:  "inproc（内嵌后台）",
		Daemon:  d,
	}, "", nil
}

func describeLocal(s *Session) string {
	if s == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := s.Client.Status(ctx)
	if err != nil {
		return "本地数据目录: " + s.DataDir
	}
	network := ""
	if st.Network != nil {
		network = *st.Network
	}
	address := ""
	if st.Address != nil {
		address = *st.Address
	}
	parts := []string{"本地数据目录: " + s.DataDir}
	if network != "" {
		parts = append(parts, "网络: "+network)
	}
	if address != "" {
		parts = append(parts, "地址: "+address)
	}
	return strings.Join(parts, "，")
}
