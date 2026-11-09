// Package apiclient is the CLI HTTP client for the km daemon. The CLI and
// the Web UI use the same HTTP API over 127.0.0.1 (or an explicit --api URL
// for remote access). There is no token: loopback access relies on file
// permissions and user-owned reverse proxies for remote exposure.
//
// Both transports share the same API surface:
//   - external daemon: standard HTTP over TCP;
//   - embedded daemon: in-process RoundTripper over Daemon.Handler().
//
// Timeouts are per-request, never a single total timeout for SSE:
//   - probe (status): 3s;
//   - normal business: 10s;
//   - unlock / task submit (KDF, retry drain): 30s;
//   - SSE: follows the caller context with no fixed timeout.
package apiclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ProbeTimeout bounds external daemon discovery.
	ProbeTimeout = 3 * time.Second
	// ShortTimeout bounds normal business requests.
	ShortTimeout = 10 * time.Second
	// LongTimeout bounds KDF and retry-drain requests.
	LongTimeout = 30 * time.Second
)

// TransportError reports that no HTTP response was received: connection
// refused, DNS failure, timeout, or caller cancellation. It never carries
// a business response, so callers must not mistake it for one.
type TransportError struct {
	Op  string
	Err error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("连接后台失败 (%s): %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// HTTPError reports a business response with a non-2xx status. The server
// was reached; the request must not be retried against another backend.
type HTTPError struct {
	Op     string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("后台返回 %d: %s", e.Status, e.Body)
}

func transportErr(op string, err error) error {
	return &TransportError{Op: op, Err: err}
}

func httpErr(op string, status int, body string) error {
	return &HTTPError{Op: op, Status: status, Body: strings.TrimSpace(body)}
}

// Client talks to one daemon base URL.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for baseURL (trailing slash trimmed). A nil http
// client uses a transport without a total timeout; per-request context
// deadlines apply instead so SSE streams are not cut by a shared timeout.
func New(baseURL string, client *http.Client) *Client {
	if client == nil {
		client = &http.Client{}
	}
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		HTTP:    client,
	}
}

// NewExternal returns a standard TCP client for an external daemon.
func NewExternal(baseURL string) *Client {
	return New(baseURL, &http.Client{})
}

// ResolveBaseURL implements the CLI address priority: explicit --api wins;
// otherwise read the local settings.json port and dial 127.0.0.1:<port>.
// Without configuration and without --api it errors instead of initializing
// a wallet or starting a second daemon.
func ResolveBaseURL(explicitAPI, dataDir string) (string, error) {
	if strings.TrimSpace(explicitAPI) != "" {
		return strings.TrimSuffix(strings.TrimSpace(explicitAPI), "/"), nil
	}
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
		return "", fmt.Errorf("无本机配置且未提供 --api，请提供 --api 或先运行 km init/daemon: %w", err)
	}
	var settings struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return "", fmt.Errorf("读取本机配置失败: %w", err)
	}
	if settings.Port == 0 {
		settings.Port = 8790
	}
	return fmt.Sprintf("http://127.0.0.1:%d", settings.Port), nil
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d)
}

func (c *Client) getWithContext(ctx context.Context, path string, out any) error {
	op := "GET " + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportErr(op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return httpErr(op, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析后台响应失败: %w", err)
	}
	return nil
}

func (c *Client) postWithContext(ctx context.Context, path string, body any, out any) error {
	op := "POST " + path
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportErr(op, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return httpErr(op, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析后台响应失败: %w", err)
	}
	return nil
}

// Status is GET /api/status with probe timeout.
func (c *Client) Status(ctx context.Context) (map[string]any, error) {
	ctx, cancel := withTimeout(ctx, ProbeTimeout)
	defer cancel()
	var out map[string]any
	if err := c.getWithContext(ctx, "/api/status", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Info is GET /api/info with normal business timeout.
func (c *Client) Info(ctx context.Context) (map[string]any, error) {
	ctx, cancel := withTimeout(ctx, ShortTimeout)
	defer cancel()
	var out map[string]any
	if err := c.getWithContext(ctx, "/api/info", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Unlock posts the password to /api/unlock with KDF-tolerant timeout.
// The caller owns clearing the password buffer.
func (c *Client) Unlock(ctx context.Context, password string) error {
	ctx, cancel := withTimeout(ctx, LongTimeout)
	defer cancel()
	return c.postWithContext(ctx, "/api/unlock", map[string]string{"password": password}, nil)
}

// CreateTask posts a download task (idempotent per seed) with retry-drain
// tolerant timeout. Request cancellation does not cancel the task itself.
func (c *Client) CreateTask(ctx context.Context, seed string) (map[string]any, error) {
	ctx, cancel := withTimeout(ctx, LongTimeout)
	defer cancel()
	var out map[string]any
	if err := c.postWithContext(ctx, "/api/tasks", map[string]string{"seedHashHex": seed}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTask polls one task with normal business timeout.
func (c *Client) GetTask(ctx context.Context, seed string) (map[string]any, error) {
	ctx, cancel := withTimeout(ctx, ShortTimeout)
	defer cancel()
	var out map[string]any
	if err := c.getWithContext(ctx, "/api/tasks/"+seed, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CancelTask explicitly cancels a task (bounded close, evidence kept).
// Request cancellation does not imply task cancellation; only this API does.
func (c *Client) CancelTask(ctx context.Context, seed string) error {
	ctx, cancel := withTimeout(ctx, ShortTimeout)
	defer cancel()
	return c.postWithContext(ctx, "/api/tasks/"+seed+"/cancel", nil, nil)
}

// TaskEvents streams /api/tasks/<seed>/events (SSE) until ctx ends, the
// server closes the stream, or fn returns false. No fixed timeout applies;
// the caller context owns the lifecycle.
func (c *Client) TaskEvents(ctx context.Context, seed string, fn func(task map[string]any) bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return fmt.Errorf("TaskEvents 需要回调函数")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/tasks/"+seed+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return transportErr("GET /api/tasks/"+seed+"/events", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return httpErr("GET /api/tasks/"+seed+"/events", resp.StatusCode, string(raw))
	}
	reader := bufio.NewReader(resp.Body)
	var data strings.Builder
	flushEvent := func() error {
		if data.Len() == 0 {
			return nil
		}
		raw := strings.TrimSpace(data.String())
		data.Reset()
		if raw == "" {
			return nil
		}
		var task map[string]any
		if err := json.Unmarshal([]byte(raw), &task); err != nil {
			return fmt.Errorf("解析进度流失败: %w", err)
		}
		if !fn(task) {
			return io.EOF
		}
		return nil
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors_IsEOF(err) && data.Len() == 0 {
				return nil
			}
			// Server closed after terminal state: treat buffered event first.
			if data.Len() > 0 {
				if ferr := flushEvent(); ferr != nil {
					if ferr == io.EOF {
						return nil
					}
					return ferr
				}
			}
			if errors_IsEOF(err) || errors_IsCanceled(ctx) {
				return nil
			}
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, ":") {
			// SSE heartbeat/comment.
			continue
		}
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if data.Len() > 0 {
				data.WriteString("\n")
			}
			data.WriteString(payload)
			continue
		}
		if trimmed == "" {
			if err := flushEvent(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		}
	}
}

func errors_IsEOF(err error) bool {
	return err == io.EOF
}

func errors_IsCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
