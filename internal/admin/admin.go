// Package admin serves the local Web Admin API and its embedded single-page
// UI.
//
// It only starts after the CLI has unlocked the wallet; it can read and change
// catalog pricing but can never unlock the wallet or sign anything. Requests
// must present the configured API key, a localhost Host header and a
// same-origin (or absent) Origin header.
package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
	"github.com/bsv8/keymaster.cc-cli/internal/pricing"
)

//go:embed ui.html
var uiFS embed.FS

var uiTemplate = template.Must(template.ParseFS(uiFS, "ui.html"))

// Options configures the admin server.
type Options struct {
	// Address is the listen address, for example 127.0.0.1:8790.
	Address string
	// APIKey is the required bearer token; empty disables the server.
	APIKey string
	// Network is the configured BSV network ("main" or "test").
	Network string
	// Address_ is the wallet receiving address shown in the status view.
	WalletAddress string
	// Catalog 统一查询磁盘目录和持久内容映射，不持有扫描快照。
	Catalog *catalog.Catalog
	Pricing *pricing.Pricing
	// Status optionally contributes extra status fields.
	Status func() map[string]any
	// PersistDefaults saves new server default prices to the settings
	// file (config.json). It runs after the in-memory/pricing.json update
	// validates; a nil value leaves PUT /api/pricing/defaults unavailable.
	PersistDefaults func(seedPrice, blockPrice string) error
}

// Server is a running admin HTTP server.
type Server struct {
	options    Options
	server     *http.Server
	addr       string
	defaultsMu sync.Mutex
}

// Start validates options and serves in a goroutine.
func Start(ctx context.Context, options Options) (*Server, error) {
	if options.APIKey == "" {
		return nil, errors.New("admin: api key is required")
	}
	if options.Address == "" {
		options.Address = "127.0.0.1:8790"
	}
	// The admin surface must never be exposed beyond this machine; Host header
	// validation is defence in depth, not a substitute for the listen address.
	if host, _, err := net.SplitHostPort(options.Address); err != nil {
		return nil, fmt.Errorf("admin: invalid listen address: %w", err)
	} else if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return nil, fmt.Errorf("admin: listen address must be loopback, got %q", host)
	}
	if options.Catalog == nil || options.Pricing == nil {
		return nil, errors.New("admin: index and pricing are required")
	}
	mux := http.NewServeMux()
	server := &Server{options: options}
	mux.HandleFunc("/", server.handleUI)
	mux.HandleFunc("/api/status", server.handleStatus)
	mux.HandleFunc("/api/files", server.handleFiles)
	mux.HandleFunc("/api/catalog/refresh", server.handleRefresh)
	mux.HandleFunc("/api/pricing/batch", server.handleBatch)
	mux.HandleFunc("/api/pricing/defaults", server.handleDefaults)
	mux.HandleFunc("/api/pricing/file", server.handleFile)
	server.server = &http.Server{Addr: options.Address, Handler: server.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	listener, err := net.Listen("tcp", options.Address)
	if err != nil {
		return nil, fmt.Errorf("admin: listen: %w", err)
	}
	server.addr = listener.Addr().String()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.server.Shutdown(shutdown)
	}()
	go func() {
		_ = server.server.Serve(listener)
	}()
	return server, nil
}

// Addr returns the actual listen address.
func (s *Server) Addr() string {
	if s.server == nil {
		return ""
	}
	return s.addr
}

// Close stops the server.
func (s *Server) Close() error {
	if s.server == nil {
		return nil
	}
	return s.server.Close()
}

// guard enforces API key, Host and Origin rules before any handler.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host := request.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(writer, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := request.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Host != request.Host {
				http.Error(writer, "forbidden origin", http.StatusForbidden)
				return
			}
		}
		if request.URL.Path == "/" {
			next.ServeHTTP(writer, request)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		if token == "" {
			token = request.Header.Get("X-API-Key")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.options.APIKey)) != 1 {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) handleUI(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("content-type", "text/html; charset=utf-8")
	// The page is a login shell: it carries no network, wallet or file
	// data. Everything sensitive loads after the key verifies against
	// /api/status.
	_ = uiTemplate.Execute(writer, nil)
}

func (s *Server) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}
	entries, err := s.options.Catalog.List(request.Context())
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	seeds := map[string]bool{}
	ready, failed := 0, 0
	for _, entry := range entries {
		if entry.State == "ready" {
			ready++
			seeds[entry.SeedHashHex] = true
		}
		if entry.State == "failed" {
			failed++
		}
	}
	seedPrice, blockPrice := s.options.Pricing.Defaults()
	status := map[string]any{
		"network":        s.options.Network,
		"wallet":         s.options.WalletAddress,
		"files":          len(entries),
		"seeds":          len(seeds),
		"readyFiles":     ready,
		"failedFiles":    failed,
		"unindexedFiles": len(entries) - ready - failed,
		"seedPrice":      seedPrice,
		"blockPrice":     blockPrice,
		"pricedFiles":    len(s.options.Pricing.Files()),
	}
	if s.options.Status != nil {
		for key, value := range s.options.Status() {
			status[key] = value
		}
	}
	writeJSON(writer, status)
}

type fileView struct {
	Path       string  `json:"path"`
	Name       string  `json:"name"`
	Size       *uint64 `json:"size"`
	SeedHash   string  `json:"seedHash"`
	Blocks     *uint64 `json:"blocks"`
	SeedPrice  string  `json:"seedPrice"`
	BlockPrice string  `json:"blockPrice"`
	Override   bool    `json:"override"`
	State      string  `json:"state"`
	Error      string  `json:"error,omitempty"`
}

func (s *Server) handleFiles(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer)
		return
	}
	entries, err := s.options.Catalog.List(request.Context())
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	files := make([]fileView, 0, len(entries))
	for _, entry := range entries {
		seedPrice, blockPrice, override := s.options.Pricing.Effective(entry.RelativePath)
		var blocks, size *uint64
		if entry.State == "ready" {
			count := entry.BlockCount
			blocks = &count
		}
		if entry.Size > 0 {
			value := entry.Size
			size = &value
		}
		files = append(files, fileView{
			Path: entry.RelativePath, Name: entry.Name, Size: size,
			SeedHash: entry.SeedHashHex, Blocks: blocks,
			SeedPrice: seedPrice, BlockPrice: blockPrice, Override: override,
			State: entry.State, Error: entry.Error,
		})
	}
	writeJSON(writer, map[string]any{"files": files})
}

// handleRefresh 与 SIGHUP、周期核对共用 Catalog 的串行刷新入口。
func (s *Server) handleRefresh(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024)).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}
	report, err := s.options.Catalog.Refresh(request.Context(), body.Force)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, report)
}

type batchRequest struct {
	Mode string `json:"mode"`
	// Path optionally limits the batch to one directory and its children
	// (slash-separated, relative to the server root). Empty means the whole
	// index. Prices are still written per file, never per directory.
	Path       string `json:"path"`
	SeedPrice  string `json:"seedPrice"`
	BlockPrice string `json:"blockPrice"`
}

func (s *Server) handleBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer)
		return
	}
	var body batchRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}
	prefix := strings.TrimSuffix(strings.TrimSpace(body.Path), "/")
	if prefix != "" {
		cleaned := path.Clean(prefix)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
			http.Error(writer, "path must be a relative directory", http.StatusBadRequest)
			return
		}
		prefix = cleaned
	}
	entries, listErr := s.options.Catalog.List(request.Context())
	if listErr != nil {
		http.Error(writer, listErr.Error(), http.StatusInternalServerError)
		return
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if prefix != "" && entry.RelativePath != prefix && !strings.HasPrefix(entry.RelativePath, prefix+"/") {
			continue
		}
		paths = append(paths, entry.RelativePath)
	}
	var err error
	switch body.Mode {
	case "all":
		err = s.options.Pricing.BatchAll(paths, body.SeedPrice, body.BlockPrice)
	case "unpriced":
		err = s.options.Pricing.BatchUnpriced(paths, body.SeedPrice, body.BlockPrice)
	default:
		http.Error(writer, "mode must be all or unpriced", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(writer, map[string]any{"updated": len(paths)})
}

// handleDefaults updates the server default prices: validation against
// the pricing table first (400 on bad sats), then the settings file
// (500 on persistence failure). A persistence failure after the in-memory
// update is reported as 500 and converges back on restart, which always
// applies the settings file.
func (s *Server) handleDefaults(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPut {
		methodNotAllowed(writer)
		return
	}
	var body struct {
		SeedPrice  string `json:"seedPrice"`
		BlockPrice string `json:"blockPrice"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(writer, "invalid json", http.StatusBadRequest)
		return
	}
	if s.options.PersistDefaults == nil {
		http.Error(writer, "defaults persistence is not configured", http.StatusInternalServerError)
		return
	}
	s.defaultsMu.Lock()
	defer s.defaultsMu.Unlock()
	if err := s.options.Pricing.SetDefaults(body.SeedPrice, body.BlockPrice); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.options.PersistDefaults(body.SeedPrice, body.BlockPrice); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(writer, map[string]any{"seedPrice": body.SeedPrice, "blockPrice": body.BlockPrice})
}

type fileRequest struct {
	Path       string `json:"path"`
	SeedPrice  string `json:"seedPrice"`
	BlockPrice string `json:"blockPrice"`
}

func (s *Server) handleFile(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodPut, http.MethodDelete:
	default:
		methodNotAllowed(writer)
		return
	}
	var body fileRequest
	if request.Method == http.MethodPut {
		if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(writer, "invalid json", http.StatusBadRequest)
			return
		}
		if err := s.options.Pricing.SetFile(body.Path, body.SeedPrice, body.BlockPrice); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(writer, map[string]any{"updated": body.Path})
		return
	}
	path := request.URL.Query().Get("path")
	if path == "" {
		http.Error(writer, "path is required", http.StatusBadRequest)
		return
	}
	if err := s.options.Pricing.ClearFile(path); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(writer, map[string]any{"cleared": path})
}

func writeJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("content-type", "application/json")
	writer.Header().Set("cache-control", "no-store")
	_ = json.NewEncoder(writer).Encode(value)
}

func methodNotAllowed(writer http.ResponseWriter) {
	writer.Header().Set("allow", "GET, POST, PUT, DELETE")
	http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
}
