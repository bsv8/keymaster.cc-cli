package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/bsv8/keymaster.cc-cli/internal/apiclient"
	"github.com/bsv8/keymaster.cc-cli/internal/daemon"
	"github.com/bsv8/keymaster.cc-cli/internal/dlock"
	"github.com/bsv8/keymaster.cc-cli/internal/inprocapi"
)

// Session unifies external and embedded daemon access. Commands only use
// Session.Client; Session owns cleanup.
type Session struct {
	Client    *apiclient.Client
	Mode      string // "external" or "embedded"
	DataDir   string
	APIURL    string
	Daemon    *daemon.Daemon
	transport *inprocapi.Transport
}

// Close releases session resources. External mode only closes idle
// connections; embedded mode stops the embedded daemon with bounded
// cleanup, clears keys and releases the data directory lock.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	if s.Mode == "embedded" && s.Daemon != nil {
		if s.transport != nil {
			s.transport.Close()
		}
		return s.Daemon.Close()
	}
	if s.Client != nil && s.Client.HTTP != nil {
		s.Client.HTTP.CloseIdleConnections()
	}
	return nil
}

// OpenSession probes an external daemon first and falls back to an embedded
// one. It never creates a wallet and never starts a second daemon when the
// data directory lock is held.
//
// Rules (locked contract):
//   - candidate priority: explicit --api, then local settings.json port,
//     then local-init check;
//   - probe via /api/status with product/apiVersion validation;
//   - no address / connection refused / timeout -> try embedded;
//   - explicit remote unreachable also falls back, but warns with local
//     data dir / network / address;
//   - daemon reachable but locked -> return external (caller unlocks),
//     never start a second daemon;
//   - business error, corrupt config, illegal URL, protocol mismatch ->
//     explicit error, no fallback;
//   - request already sent then timeout/disconnect is handled by callers
//     (query original task, never repurchase here).
func OpenSession(ctx context.Context, explicitAPI, dataDir string) (*Session, string, error) {
	layout, err := resolveLayout(dataDir)
	if err != nil {
		return nil, "", err
	}

	candidate, resolveErr := apiclient.ResolveBaseURL(explicitAPI, layout.Dir)
	if resolveErr == nil {
		if err := validateAPIURL(candidate); err != nil {
			return nil, "", err
		}
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		probe := apiclient.NewExternal(candidate)
		st, perr := probe.Status(ctx)
		if perr == nil {
			if err := checkDaemonIdentity(st); err != nil {
				return nil, "", err
			}
			return &Session{
				Client:  probe,
				Mode:    "external",
				DataDir: layout.Dir,
				APIURL:  candidate,
			}, "", nil
		}
		if err := ctx.Err(); err != nil {
			// Caller cancelled (Ctrl-C) during probe: respect it, never
			// start an embedded daemon as a side effect.
			return nil, "", err
		}
		if !isConnectionFailure(perr) {
			// Business error or illegal URL/protocol: explicit, no fallback.
			return nil, "", perr
		}
		// Connection failure: fall through to embedded with warning.
		warning := fmt.Sprintf("外部后台不可达 (%v)，改用本地内嵌后台", perr)
		sess, err := openEmbedded(ctx, layout.Dir)
		if err != nil {
			return nil, "", fmt.Errorf("外部后台不可达 (%v)；%w", perr, err)
		}
		extra := describeLocal(sess)
		if extra != "" {
			warning += "；" + extra
		}
		if strings.TrimSpace(explicitAPI) != "" {
			warning += "（显式 --api 未连接，当前操作的是本地钱包，非远程）"
		}
		return sess, warning, nil
	}

	// No candidate: distinguish missing config (try embedded) from corrupt
	// config (explicit error).
	msg := resolveErr.Error()
	if strings.Contains(msg, "读取本机配置失败") {
		return nil, "", resolveErr
	}
	// Missing local config without --api: check local init, then embedded.
	sess, w, err := openEmbeddedOrInitHint(ctx, layout.Dir)
	if err != nil {
		// Preserve the original resolve hint when local is also unusable.
		if sess == nil {
			_, _ = fmt.Fprintf(os.Stderr, "")
		}
		return nil, "", err
	}
	return sess, w, nil
}

// validateAPIURL rejects unsupported schemes and malformed addresses before
// any probe, so they surface as explicit errors instead of falling back to
// the local wallet.
func validateAPIURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("API 地址非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("API 地址协议不支持 %q，只支持 http/https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("API 地址缺少主机: %q", raw)
	}
	return nil
}

func checkDaemonIdentity(st map[string]any) error {
	if st == nil {
		return fmt.Errorf("后台协议不兼容：状态为空")
	}
	// Both fields are mandatory: an empty map (or a foreign program on the
	// same port) must never be mistaken for a km daemon.
	rawProd, ok := st["product"]
	if !ok || rawProd == nil {
		return fmt.Errorf("后台协议不兼容：缺少 product 标识，需要 %q", daemon.DaemonProduct)
	}
	prod, ok := rawProd.(string)
	if !ok || prod == "" {
		return fmt.Errorf("后台协议不兼容：product 非法，需要 %q", daemon.DaemonProduct)
	}
	if prod != daemon.DaemonProduct {
		return fmt.Errorf("后台协议不兼容：product=%q，需要 %q", prod, daemon.DaemonProduct)
	}
	rawVer, ok := st["apiVersion"]
	if !ok || rawVer == nil {
		return fmt.Errorf("后台协议不兼容：缺少 apiVersion，需要 %q", daemon.DaemonAPIVersion)
	}
	ver, ok := rawVer.(string)
	if !ok || ver == "" {
		return fmt.Errorf("后台协议不兼容：apiVersion 非法，需要 %q", daemon.DaemonAPIVersion)
	}
	if ver != daemon.DaemonAPIVersion {
		return fmt.Errorf("后台协议不兼容：apiVersion=%q，需要 %q", ver, daemon.DaemonAPIVersion)
	}
	return nil
}

// isConnectionFailure reports transport-level failures (refused, timeout,
// DNS) that justify falling back to an embedded daemon. Business HTTP
// responses, protocol mismatches and caller cancellation never fall back.
func isConnectionFailure(err error) bool {
	if err == nil {
		return false
	}
	// Caller cancellation is explicit, never a reason to start a wallet.
	if errors.Is(err, context.Canceled) {
		return false
	}
	var terr *apiclient.TransportError
	if errors.As(err, &terr) {
		// A transport error wrapping cancellation (e.g. probe ctx) is
		// still a cancellation, not an offline daemon.
		if errors.Is(terr.Err, context.Canceled) {
			return false
		}
		return true
	}
	// Defensive: raw context timeouts without wrapper.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return false
}

func openEmbeddedOrInitHint(ctx context.Context, dir string) (*Session, string, error) {
	layout, err := resolveLayout(dir)
	if err != nil {
		return nil, "", err
	}
	if !layout.HasInitMarkers() {
		if layout.HasAnyWallet() {
			return nil, "", fmt.Errorf("数据不完整或为旧版，请先运行 km init（迁移或修复）")
		}
		return nil, "", fmt.Errorf("尚未初始化，请先运行 km init")
	}
	return openEmbeddedWithWarning(ctx, layout.Dir, "")
}

func openEmbedded(ctx context.Context, dir string) (*Session, error) {
	sess, _, err := openEmbeddedWithWarning(ctx, dir, "")
	return sess, err
}

func openEmbeddedWithWarning(ctx context.Context, dir string, _ string) (*Session, string, error) {
	layout, err := resolveLayout(dir)
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
	// Ensure cleanup on every failure after Open.
	success := false
	defer func() {
		if !success {
			_ = d.Close()
		}
	}()
	if err := d.Start(ctx); err != nil {
		return nil, "", err
	}
	// Password entry honors command cancellation (Ctrl-C): the terminal
	// state is restored and the just-opened daemon is closed (releasing
	// the data directory lock) instead of hanging.
	password, err := readHiddenPasswordWithContext(ctx, "钱包密码: ")
	if err != nil {
		return nil, "", err
	}
	if password == "" {
		return nil, "", fmt.Errorf("密码不能为空")
	}
	if err := d.Unlock(password); err != nil {
		return nil, "", err
	}
	tr := &inprocapi.Transport{Handler: d.Handler()}
	httpClient := &http.Client{Transport: tr}
	client := apiclient.New(inprocapi.BaseURL, httpClient)
	// Validate the embedded surface matches the external one.
	if st, err := client.Status(ctx); err != nil {
		return nil, "", fmt.Errorf("内嵌后台自检失败: %w", err)
	} else if err := checkDaemonIdentity(st); err != nil {
		return nil, "", err
	}
	success = true
	return &Session{
		Client:    client,
		Mode:      "embedded",
		DataDir:   layout.Dir,
		APIURL:    inprocapi.BaseURL + "（内嵌后台）",
		Daemon:    d,
		transport: tr,
	}, "", nil
}

func describeLocal(s *Session) string {
	if s == nil || s.Daemon == nil {
		return ""
	}
	// Best-effort: report local data dir via session; network/address come
	// from the embedded status call when available.
	ctx, cancel := context.WithTimeout(context.Background(), apiclient.ShortTimeout)
	defer cancel()
	st, err := s.Client.Status(ctx)
	if err != nil {
		return fmt.Sprintf("本地数据目录: %s", s.DataDir)
	}
	network, _ := st["network"].(string)
	address, _ := st["address"].(string)
	parts := []string{fmt.Sprintf("本地数据目录: %s", s.DataDir)}
	if network != "" {
		parts = append(parts, fmt.Sprintf("网络: %s", network))
	}
	if address != "" {
		parts = append(parts, fmt.Sprintf("地址: %s", address))
	}
	return strings.Join(parts, "，")
}

// tryLockHeld reports whether another process currently holds the data
// directory lock. It never steals the lock: a successful probe is released
// immediately.
func tryLockHeld(dir string) bool {
	guard, err := dlock.Acquire(dir)
	if err != nil {
		return errors.Is(err, dlock.ErrActive)
	}
	_ = guard.Release()
	return false
}
