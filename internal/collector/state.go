package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// StateVersion 是持久化文档版本。
const StateVersion = 1

// Inflight 是发送中批次：固定的 gzip、签名与该批记录版本。
type Inflight struct {
	Gzip      []byte            `json:"gzip"`
	Signature []byte            `json:"signature"`
	PubkeyHex string            `json:"pubkeyHex"`
	Records   map[string]Record `json:"records"`
	Trigger   string            `json:"trigger"`
}

// Meta 是提交进度水位。
type Meta struct {
	LastAttemptAt int64  `json:"lastAttemptAt"`
	LastSuccessAt *int64 `json:"lastSuccessAt"`
	LastError     string `json:"lastError"`
}

// State 是采集持久化文档：确认态、待提交队列、发送中批次、介绍。
// 与 Catalog 内容映射分开存放（~/.km/server/collector.json）。
type State struct {
	Version      int               `json:"version"`
	Descriptions map[string]string `json:"descriptions"`
	Confirmed    map[string]Record `json:"confirmed"`
	Pending      map[string]Record `json:"pending"`
	Inflight     *Inflight         `json:"inflight"`
	Meta         Meta              `json:"meta"`
}

func freshState() *State {
	return &State{
		Version:      StateVersion,
		Descriptions: map[string]string{},
		Confirmed:    map[string]Record{},
		Pending:      map[string]Record{},
	}
}

// Store 串行化读写采集状态，原子 0600 落盘。
type Store struct {
	path string
	mu   sync.Mutex
	doc  *State
}

// Open 读取或初始化采集状态。
func Open(path string) (*Store, error) {
	s := &Store{path: path, doc: freshState()}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("collector: 读取状态失败: %w", err)
	}
	var doc State
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("collector: 解析状态失败: %w", err)
	}
	if doc.Version != StateVersion {
		return s, nil
	}
	if doc.Descriptions == nil {
		doc.Descriptions = map[string]string{}
	}
	if doc.Confirmed == nil {
		doc.Confirmed = map[string]Record{}
	}
	if doc.Pending == nil {
		doc.Pending = map[string]Record{}
	}
	if doc.Inflight != nil && (len(doc.Inflight.Gzip) == 0 || len(doc.Inflight.Signature) == 0) {
		doc.Inflight = nil
	}
	s.doc = &doc
	return s, nil
}

// Update 原子读改写：mutate 内同步修改，保存后返回其返回值。
func (s *Store) Update(mutate func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := mutate(s.doc); err != nil {
		return err
	}
	return s.saveLocked()
}

// Snapshot 返回深拷贝快照。
func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.doc)
	var out State
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Store) saveLocked() error {
	encoded, err := json.Marshal(s.doc)
	if err != nil {
		return fmt.Errorf("collector: 序列化状态失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("collector: 创建状态目录失败: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".collector-*.tmp")
	if err != nil {
		return fmt.Errorf("collector: 创建临时文件失败: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("collector: 写入状态失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("collector: 替换状态失败: %w", err)
	}
	return nil
}
