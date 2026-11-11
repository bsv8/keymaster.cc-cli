package appstorage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// errClosing 在关闭流程开始后拒绝新存储操作。
var errClosing = errors.New("appstorage: 正在关闭，不再接受新操作")

// Layout 为独立 Storage 根布局，与钱包根完全分开。
type Layout struct {
	Dir     string
	Lock    string
	DBPath  string
	WALPath string
	SHMPath string
}

// DefaultDir 返回默认 Storage 根 ~/.km-storage。
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("appstorage: 无法定位用户主目录: %w", err)
	}
	return filepath.Join(home, ".km-storage"), nil
}

// Resolve 解析 Storage 根：显式优先，否则钱包根同级 <目录名>-storage，默认钱包根为空时用默认根。
func Resolve(walletDir, explicit string) (Layout, error) {
	var dir string
	if strings.TrimSpace(explicit) != "" {
		dir = explicit
	} else if strings.TrimSpace(walletDir) != "" {
		abs, err := filepath.Abs(walletDir)
		if err != nil {
			return Layout{}, fmt.Errorf("appstorage: 解析钱包目录失败: %w", err)
		}
		dir = abs + "-storage"
	} else {
		d, err := DefaultDir()
		if err != nil {
			return Layout{}, err
		}
		dir = d
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Layout{}, fmt.Errorf("appstorage: 解析 Storage 目录失败: %w", err)
	}
	return Layout{
		Dir:     abs,
		Lock:    filepath.Join(abs, "lock"),
		DBPath:  filepath.Join(abs, "objects.sqlite"),
		WALPath: filepath.Join(abs, "objects.sqlite-wal"),
		SHMPath: filepath.Join(abs, "objects.sqlite-shm"),
	}, nil
}

// canonicalPath 解析路径中已存在的最近祖先的符号链接，再拼回尚不存在的
// 剩余段；用于 Storage 目录尚未创建时仍能发现符号链接造成的重叠。
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	cur := abs
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return filepath.Clean(resolved)
			}
			return filepath.Clean(filepath.Join(resolved, rest))
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(abs)
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
}

// CheckOverlap 禁止相同、父子重叠和符号链接造成的重叠。
func CheckOverlap(walletDir, storageDir string) error {
	wReal := canonicalPath(walletDir)
	sReal := canonicalPath(storageDir)
	if wReal == sReal {
		return fmt.Errorf("appstorage: 钱包根与 Storage 根不得相同")
	}
	if strings.HasPrefix(sReal, wReal+string(os.PathSeparator)) || strings.HasPrefix(wReal, sReal+string(os.PathSeparator)) {
		return fmt.Errorf("appstorage: 钱包根与 Storage 根禁止父子重叠")
	}
	return nil
}

// Store 为独立 SQLite 对象库，不与钱包、任务、账本、outbox、exact、Catalog 同库。
type Store struct {
	db     *sql.DB
	layout Layout

	mu      sync.Mutex
	writeMu sync.Mutex // 统一写队列：SQLite 只允许一个写事务，避免延迟事务升级为 SQLITE_BUSY
	wg      sync.WaitGroup
	closing bool
}

// Begin 标记一次存储操作开始；关闭流程开始后拒绝新操作。
// 调用者必须配对调用 End（通常以 defer）。
func (s *Store) Begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.db == nil {
		return errClosing
	}
	s.wg.Add(1)
	return nil
}

// End 标记一次存储操作结束。
func (s *Store) End() { s.wg.Done() }

// BeginClose 进入关闭流程：拒绝新操作，返回等待在途操作完成的通道函数。
// 调用者应在有界超时内等待 Wait，超时则视为异常收尾，不得释放独占锁。
func (s *Store) BeginClose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
}

// Wait 等待在途操作排空。
func (s *Store) Wait() { s.wg.Wait() }

// Open 打开或创建对象库并建表。
func Open(layout Layout) (*Store, error) {
	if err := os.MkdirAll(layout.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("appstorage: 创建目录失败: %w", err)
	}
	db, err := sql.Open("sqlite", layout.DBPath)
	if err != nil {
		return nil, fmt.Errorf("appstorage: 打开数据库失败: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS objects (
		network TEXT NOT NULL,
		wallet_pubkey TEXT NOT NULL,
		app_pubkey TEXT NOT NULL,
		path TEXT NOT NULL,
		content BLOB NOT NULL,
		mime TEXT NOT NULL,
		revision INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (network, wallet_pubkey, app_pubkey, path)
	); CREATE INDEX IF NOT EXISTS idx_objects_prefix ON objects(network, wallet_pubkey, app_pubkey, path);
	CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
	INSERT OR IGNORE INTO meta(key,value) VALUES('revision_seq',0);
	UPDATE meta SET value = MAX(value, (SELECT COALESCE(MAX(revision),0) FROM objects)) WHERE key='revision_seq';`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, layout: layout}, nil
}

// Close 关闭数据库；调用者保证无事务仍在使用资源。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// logicalKey 构造逻辑键（仅用于日志，不作存储键）。
func LogicalKey(network, wallet, app, path string) string {
	return network + "/" + wallet + "/" + app + "/files/" + strings.TrimPrefix(path, "/")
}
