package connect

import (
	"encoding/json"
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/walletcrypto"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// APPROVAL_METHODS 为需要用户逐次确认的五类操作，与 keymaster.cc applicationPermissions 一致。
var ApprovalMethods = []string{"identity.get", "intent.sign", "cipher.encrypt", "cipher.decrypt", "payment.pay"}

// copyStrings 返回非 nil 副本，确保 JSON 将空集合编码为 [] 而不是 null。
func copyStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func IsApprovalMethod(method string) bool {
	for _, m := range ApprovalMethods {
		if m == method {
			return true
		}
	}
	return false
}

// ApplicationKey 只接受已验证的 App 公钥，不提供来源身份回退。
func ApplicationKey(appPublicKeyHex string) string {
	key, err := walletcrypto.ValidateAppPublicKeyHex(appPublicKeyHex)
	if err != nil {
		return ""
	}
	return key
}

// PermissionEntry 为单个 App 公钥的自动许可快照；Origin 仅为最近操作来源。
type PermissionEntry struct {
	AppKey    string   `json:"appKey"`
	Origin    string   `json:"origin"`
	Methods   []string `json:"methods"`
	UpdatedAt int64    `json:"updatedAt"`
}

// HistoryEvent 为应用连接/审批/许可的可审计事件。
// 不含密钥、token、解密明文或完整原始报文，仅保留关联、状态与错误码。
type HistoryEvent struct {
	ID     string `json:"id"`
	At     int64  `json:"at"`
	AppKey string `json:"appKey"`
	Origin string `json:"origin"`
	// Kind: connected/disconnected/permission-granted/permission-revoked/
	//       manual-approved/auto-approved/executed/failed/user-canceled/timed-out
	Kind      string `json:"kind"`
	Method    string `json:"method,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

const maxHistoryEvents = 2000

type AppState struct {
	mu     sync.Mutex
	path   string
	perms  map[string]*PermissionEntry
	events []HistoryEvent
	seq    int64
}

type appStateFile struct {
	Permissions []PermissionEntry `json:"permissions"`
	Events      []HistoryEvent    `json:"events"`
}

func NewAppState(pluginDir string) *AppState {
	return &AppState{path: filepath.Join(pluginDir, "apps.json"), perms: map[string]*PermissionEntry{}}
}

func (s *AppState) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	var f appStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	m := map[string]*PermissionEntry{}
	for _, e := range f.Permissions {
		if ApplicationKey(e.AppKey) == "" {
			return fmt.Errorf("incompatible application permission identity")
		}
		cp := e
		cp.Methods = copyStrings(e.Methods)
		m[e.AppKey] = &cp
	}
	for _, event := range f.Events {
		if ApplicationKey(event.AppKey) == "" {
			return fmt.Errorf("incompatible application history identity")
		}
	}
	s.perms = m
	s.events = append([]HistoryEvent(nil), f.Events...)
	return nil
}

func (s *AppState) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	plist := make([]PermissionEntry, 0, len(s.perms))
	for _, e := range s.perms {
		cp := *e
		cp.Methods = copyStrings(e.Methods)
		sort.Strings(cp.Methods)
		plist = append(plist, cp)
	}
	sort.Slice(plist, func(i, j int) bool { return plist[i].AppKey < plist[j].AppKey })
	raw, err := json.MarshalIndent(appStateFile{Permissions: plist, Events: s.events}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Methods 返回该应用已授予自动许可的操作列表。
func (s *AppState) Methods(appKey string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms == nil {
		return nil, fmt.Errorf("permission storage unavailable")
	}
	e, ok := s.perms[appKey]
	if !ok {
		return []string{}, nil
	}
	return copyStrings(e.Methods), nil
}

func (s *AppState) Has(appKey, method string) (bool, error) {
	methods, err := s.Methods(appKey)
	if err != nil {
		return false, err
	}
	for _, m := range methods {
		if m == method {
			return true, nil
		}
	}
	return false, nil
}

// Snapshot 返回全部许可快照（设置页展示用，不含秘密）。
func (s *AppState) Snapshot() []PermissionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PermissionEntry, 0, len(s.perms))
	for _, e := range s.perms {
		cp := *e
		cp.Methods = copyStrings(e.Methods)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AppKey < out[j].AppKey })
	return out
}

// Append 追加历史事件；单文件单次落盘，失败回滚内存。
func (s *AppState) Append(e HistoryEvent) (HistoryEvent, error) {
	if ApplicationKey(e.AppKey) == "" {
		return HistoryEvent{}, fmt.Errorf("verified App public key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.At == 0 {
		e.At = time.Now().UnixMilli()
	}
	s.seq++
	if e.ID == "" {
		e.ID = fmt.Sprintf("he_%d_%d", e.At, s.seq)
	}
	prevEvents := append([]HistoryEvent(nil), s.events...)
	s.events = append(s.events, e)
	if len(s.events) > maxHistoryEvents {
		s.events = s.events[len(s.events)-maxHistoryEvents:]
	}
	if err := s.saveLocked(); err != nil {
		s.events = prevEvents
		return HistoryEvent{}, err
	}
	return e, nil
}

func (s *AppState) List(appKey string, limit int) []HistoryEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HistoryEvent, 0, len(s.events))
	for _, e := range s.events {
		if appKey == "" || e.AppKey == appKey {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At == out[j].At {
			return out[i].ID > out[j].ID
		}
		return out[i].At > out[j].At
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GrantWithEvent 在同一锁内授予单项自动许可并追加对应历史事件，一次落盘。
// 任一环节失败整体回滚，不留下无事件的许可。
func (s *AppState) GrantWithEvent(appKey, method string, event HistoryEvent) error {
	if !IsApprovalMethod(method) {
		return fmt.Errorf("操作不支持自动许可")
	}
	if ApplicationKey(appKey) == "" {
		return fmt.Errorf("verified App public key is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms == nil {
		return fmt.Errorf("permission storage unavailable")
	}
	e, ok := s.perms[appKey]
	if !ok {
		e = &PermissionEntry{AppKey: appKey, Origin: event.Origin}
		s.perms[appKey] = e
	}
	for _, m := range e.Methods {
		if m == method {
			return fmt.Errorf("already granted")
		}
	}
	prevMethods := copyStrings(e.Methods)
	prevUpdated := e.UpdatedAt
	prevEvents := append([]HistoryEvent(nil), s.events...)
	e.Methods = append(e.Methods, method)
	e.UpdatedAt = time.Now().UnixMilli()
	if event.At == 0 {
		event.At = e.UpdatedAt
	}
	s.seq++
	if event.ID == "" {
		event.ID = fmt.Sprintf("he_%d_%d", event.At, s.seq)
	}
	if event.AppKey == "" {
		event.AppKey = appKey
	}
	s.events = append(s.events, event)
	if len(s.events) > maxHistoryEvents {
		s.events = s.events[len(s.events)-maxHistoryEvents:]
	}
	if err := s.saveLocked(); err != nil {
		e.Methods = prevMethods
		e.UpdatedAt = prevUpdated
		if len(prevMethods) == 0 {
			delete(s.perms, appKey)
		}
		s.events = prevEvents
		return err
	}
	return nil
}

// RevokeWithEvent 在同一锁内取消单项自动许可并追加对应历史事件，一次落盘。
// 许可不存在视为幂等成功（仍尽力追加审计事件，追加失败不视为撤销失败）。
func (s *AppState) RevokeWithEvent(appKey, method string, event HistoryEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perms == nil {
		return fmt.Errorf("permission storage unavailable")
	}
	e, ok := s.perms[appKey]
	if !ok {
		return nil
	}
	found := false
	for _, m := range e.Methods {
		if m == method {
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	prevMethods := copyStrings(e.Methods)
	prevUpdated := e.UpdatedAt
	prevEvents := append([]HistoryEvent(nil), s.events...)
	next := make([]string, 0, len(prevMethods))
	for _, m := range prevMethods {
		if m != method {
			next = append(next, m)
		}
	}
	e.Methods = next
	e.UpdatedAt = time.Now().UnixMilli()
	if event.At == 0 {
		event.At = e.UpdatedAt
	}
	s.seq++
	if event.ID == "" {
		event.ID = fmt.Sprintf("he_%d_%d", event.At, s.seq)
	}
	if event.AppKey == "" {
		event.AppKey = appKey
	}
	s.events = append(s.events, event)
	if len(s.events) > maxHistoryEvents {
		s.events = s.events[len(s.events)-maxHistoryEvents:]
	}
	if err := s.saveLocked(); err != nil {
		e.Methods = prevMethods
		e.UpdatedAt = prevUpdated
		s.events = prevEvents
		return err
	}
	return nil
}
