// Package appdeployment 管理经过整包验证的不可变应用与当前运行监听实例。
package appdeployment

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type Download func(context.Context, string) ([]byte, error)
type Record struct {
	PublicationID    string                    `json:"-"`
	AppVersion       string                    `json:"appVersion"`
	IconURL          string                    `json:"iconUrl,omitempty"`
	Display          apppublication.AppDisplay `json:"display"`
	DeploymentID     string                    `json:"deploymentId"`
	AppPublicKeyHex  string                    `json:"appPublicKeyHex"`
	Indexseedhashhex string                    `json:"indexseedhashhex"`
	Origin           string                    `json:"origin,omitempty"`
	URL              string                    `json:"url,omitempty"`
	Status           string                    `json:"status"`
	Generation       string                    `json:"-"`
}
type file struct {
	bytes []byte
	mime  string
}
type active struct {
	record   Record
	server   *http.Server
	listener net.Listener
}
type installCall struct {
	done   chan struct{}
	record Record
	err    error
}
type Manager struct {
	origins    map[string]bool
	mu         sync.Mutex
	root       string
	generation string
	download   Download
	revoke     func(Record)
	running    map[string]*active
	installs   map[string]*installCall
	closed     bool
}

func New(root string, download Download, revoke func(Record)) *Manager {
	return &Manager{origins: map[string]bool{}, root: root, generation: rand.Text(), download: download, revoke: revoke, running: map[string]*active{}, installs: map[string]*installCall{}}
}
func validSeed(seed string) bool {
	if len(seed) != 64 {
		return false
	}
	for _, c := range seed {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (m *Manager) Install(ctx context.Context, seed string) (Record, error) {
	if !validSeed(seed) {
		return Record{}, fmt.Errorf("invalid indexseedhashhex")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Record{}, fmt.Errorf("deployment manager closed")
	}
	if call := m.installs[seed]; call != nil {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.record, call.err
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
	call := &installCall{done: make(chan struct{})}
	m.installs[seed] = call
	m.mu.Unlock()
	bitfsdebug.Log("keymaster.cc-cli", "appdeployment.install.started", func() any { return map[string]any{"indexseedhashhex": seed} })
	call.record, call.err = m.install(ctx, seed)
	bitfsdebug.Log("keymaster.cc-cli", "appdeployment.install.finished", func() any { return map[string]any{"indexseedhashhex": seed, "success": call.err == nil} })
	m.mu.Lock()
	delete(m.installs, seed)
	close(call.done)
	m.mu.Unlock()
	return call.record, call.err
}
func checked(bytes []byte, seed string, size int64) error {
	_, got := apppublication.MasterSeedOfBytes(bytes)
	if got != seed || (size >= 0 && int64(len(bytes)) != size) {
		return fmt.Errorf("file size or MasterSeed mismatch")
	}
	return nil
}
func (m *Manager) install(ctx context.Context, seed string) (Record, error) {
	dest := filepath.Join(m.root, seed)
	if _, err := os.Stat(dest); err == nil {
		record, _, err := m.read(seed)
		return record, err
	} else if !os.IsNotExist(err) {
		return Record{}, err
	}
	if err := os.MkdirAll(m.root, 0700); err != nil {
		return Record{}, err
	}
	candidate, err := os.MkdirTemp(m.root, ".candidate-")
	if err != nil {
		return Record{}, err
	}
	defer os.RemoveAll(candidate)
	tree := filepath.Join(candidate, "tree")
	if err = os.Mkdir(tree, 0700); err != nil {
		return Record{}, err
	}
	entry, err := m.download(ctx, seed)
	if err != nil {
		return Record{}, err
	}
	if err = checked(entry, seed, -1); err != nil {
		return Record{}, err
	}
	payload, _, _, verifiedSeed, err := apppublication.VerifyEntryBytes(entry)
	if err != nil {
		return Record{}, err
	}
	if verifiedSeed != seed {
		return Record{}, fmt.Errorf("entry seed mismatch")
	}
	if err = os.WriteFile(filepath.Join(tree, "index.html"), entry, 0400); err != nil {
		return Record{}, err
	}
	for _, f := range payload.Files.Files {
		if err = ctx.Err(); err != nil {
			return Record{}, err
		}
		bytes, e := m.download(ctx, f.SeedHashHex)
		if e != nil {
			return Record{}, e
		}
		if e = checked(bytes, f.SeedHashHex, f.Size); e != nil {
			return Record{}, e
		}
		target := filepath.Join(tree, filepath.FromSlash(f.Path))
		rel, e := filepath.Rel(tree, target)
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return Record{}, fmt.Errorf("unsafe manifest path")
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return Record{}, e
		}
		handle, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if e != nil {
			return Record{}, e
		}
		_, e = handle.Write(bytes)
		closeErr := handle.Close()
		if e != nil {
			return Record{}, e
		}
		if closeErr != nil {
			return Record{}, closeErr
		}
	}
	publicationID, err := apppublication.PublicationID(payload)
	if err != nil {
		return Record{}, err
	}

	record := Record{PublicationID: publicationID, AppVersion: payload.AppVersion, Display: payload.FilesDisplay(), DeploymentID: seed, AppPublicKeyHex: payload.AppPublicKeyHex, Indexseedhashhex: seed, Status: "installed"}
	raw, _ := json.Marshal(record)
	if err = os.WriteFile(filepath.Join(candidate, "record.json"), raw, 0400); err != nil {
		return Record{}, err
	}
	if err = ctx.Err(); err != nil {
		return Record{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Record{}, fmt.Errorf("deployment manager closed")
	}
	if err = os.Rename(candidate, dest); err != nil {
		return Record{}, err
	}
	return record, nil
}

// read 重新验证落盘树并读取到独立字节快照；静态服务不跟随随后被改写的磁盘文件。
func (m *Manager) read(id string) (Record, map[string]file, error) {
	if !validSeed(id) {
		return Record{}, nil, fmt.Errorf("invalid deployment ID")
	}
	root := filepath.Join(m.root, id)
	tree := filepath.Join(root, "tree")
	for _, directory := range []string{root, tree} {
		stat, err := os.Lstat(directory)
		if err != nil {
			return Record{}, nil, err
		}
		if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
			return Record{}, nil, fmt.Errorf("unsafe deployment directory")
		}
	}
	if err := filepath.WalkDir(tree, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("deployment symlink forbidden")
		}
		return nil
	}); err != nil {
		return Record{}, nil, err
	}

	readFile := func(name string) ([]byte, error) {
		p := filepath.Join(root, name)
		stat, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !stat.Mode().IsRegular() {
			return nil, fmt.Errorf("deployment file is not regular")
		}
		return os.ReadFile(p)
	}
	raw, err := readFile("record.json")
	if err != nil {
		return Record{}, nil, err
	}
	var record Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return record, nil, err
	}
	if record.DeploymentID != id || record.Indexseedhashhex != id {
		return record, nil, fmt.Errorf("deployment record mismatch")
	}
	entry, err := readFile("tree/index.html")
	if err != nil {
		return record, nil, err
	}
	payload, _, _, seed, err := apppublication.VerifyEntryBytes(entry)
	if err != nil {
		return record, nil, err
	}
	if seed != id || payload.AppVersion != record.AppVersion || payload.AppPublicKeyHex != record.AppPublicKeyHex || payload.FilesDisplay() != record.Display {
		return record, nil, fmt.Errorf("deployment identity mismatch")
	}
	files := map[string]file{"index.html": {entry, "text/html"}}
	expected := map[string]bool{"index.html": true}
	for _, f := range payload.Files.Files {
		expected[f.Path] = true
		bytes, e := readFile(filepath.Join("tree", filepath.FromSlash(f.Path)))
		if e != nil {
			return record, nil, e
		}
		if e = checked(bytes, f.SeedHashHex, f.Size); e != nil {
			return record, nil, e
		}
		files[f.Path] = file{bytes, f.MIME}
	}
	err = filepath.WalkDir(tree, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("deployment symlink forbidden")
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(tree, p)
		if e != nil || !d.Type().IsRegular() || !expected[filepath.ToSlash(rel)] {
			return fmt.Errorf("unexpected deployment file")
		}
		return nil
	})
	if err != nil {
		return record, nil, err
	}
	record.PublicationID, err = apppublication.PublicationID(payload)
	if err != nil {
		return record, nil, err
	}

	record.Status = "installed"
	record.Origin = ""
	record.URL = ""
	return record, files, nil
}
func (m *Manager) Start(id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Record{}, fmt.Errorf("deployment manager closed")
	}
	if current := m.running[id]; current != nil {
		return current.record, nil
	}
	record, files, err := m.read(id)
	if err != nil {
		return Record{}, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return Record{}, err
	}
	record.Origin = "http://" + listener.Addr().String()
	record.URL = record.Origin + "/index.html"
	paths := make([]string, 0, len(files))
	for name := range files {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	for _, name := range paths {
		if name == "index.html" {
			continue
		}
		_, seed := apppublication.MasterSeedOfBytes(files[name].bytes)
		if seed == record.Display.Icon {
			record.IconURL = record.Origin + (&url.URL{Path: "/" + name}).EscapedPath()
			break
		}
	}
	if record.IconURL == "" {
		listener.Close()
		return Record{}, fmt.Errorf("verified icon seed missing")
	}
	record.Generation = m.generation + "." + rand.Text()
	record.Status = "serving"
	current := &active{record: record, listener: listener}
	current.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		valid := !m.closed && m.running[id] == current
		m.mu.Unlock()
		if !valid {
			http.Error(w, "deployment stopped", 410)
			return
		}
		if r.Host != listener.Addr().String() || (r.Method != "GET" && r.Method != "HEAD") {
			http.NotFound(w, r)
			return
		}
		f, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", f.mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == "GET" {
			_, _ = w.Write(f.bytes)
		}
	})}
	m.running[id] = current
	m.origins[record.Origin] = true
	bitfsdebug.Log("keymaster.cc-cli", "appdeployment.listen.started", func() any {
		return map[string]any{"deploymentId": id, "origin": record.Origin, "generation": record.Generation}
	})
	go func() { _ = current.server.Serve(listener); m.invalidate(id, current) }()
	return record, nil
}

// invalidate 先撤销可信登记，再通知宿主撤销令牌与运行连接，最后关闭监听。
func (m *Manager) invalidate(id string, expected *active) {
	m.mu.Lock()
	current := m.running[id]
	if current == nil || (expected != nil && current != expected) {
		m.mu.Unlock()
		return
	}
	delete(m.running, id)
	m.mu.Unlock()
	bitfsdebug.Log("keymaster.cc-cli", "appdeployment.listen.invalidated", func() any { return map[string]any{"deploymentId": id, "origin": current.record.Origin} })
	if m.revoke != nil {
		m.revoke(current.record)
	}
	_ = current.server.Close()
}
func (m *Manager) Stop(id string) error {
	if !validSeed(id) {
		return fmt.Errorf("invalid deployment ID")
	}
	m.invalidate(id, nil)
	return nil
}
func (m *Manager) Identity(origin string) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Record{}, false
	}
	for _, current := range m.running {
		if current.record.Origin == origin {
			return current.record, true
		}
	}
	return Record{}, false
}
func (m *Manager) List() ([]Record, error) {
	entries, err := os.ReadDir(m.root)
	if os.IsNotExist(err) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".candidate-") {
			continue
		}
		if !validSeed(e.Name()) || !e.IsDir() {
			return nil, fmt.Errorf("invalid deployment store")
		}
		record, _, err := m.read(e.Name())
		if err != nil {
			return nil, err
		}
		if active, ok := m.IdentityByID(e.Name()); ok {
			record = active
		}
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeploymentID < out[j].DeploymentID })
	return out, nil
}
func (m *Manager) IdentityByID(id string) (Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.running[id]; a != nil && !m.closed {
		return a.record, true
	}
	return Record{}, false
}

// RecordBySeed 读取指定 seed 的已安装部署并重验落盘，返回实际部署的公钥、
// seed 与 PublicationID，供目录启动在 Go 后台核对目录对应 Files 渠道。
func (m *Manager) RecordBySeed(seed string) (Record, error) {
	if !validSeed(seed) {
		return Record{}, fmt.Errorf("invalid indexseedhashhex")
	}
	if active, ok := m.IdentityByID(seed); ok {
		return active, nil
	}
	record, _, err := m.read(seed)
	if err != nil {
		return Record{}, err
	}
	return record, nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	ids := []string{}
	for id := range m.running {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.invalidate(id, nil)
	}
}

// WasDeploymentOrigin 阻止已失效的 Files 来源退回 URL 证明身份。
func (m *Manager) WasDeploymentOrigin(origin string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.origins[origin]
}
