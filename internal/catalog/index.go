// Package catalog 管理磁盘文件与内容哈希的持久映射，不保存第二份文件树。
package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	masterseed "github.com/bsv8/MasterSeed"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// Entry 的名称、路径和长度来自磁盘；内容身份来自已完成的 SQLite 映射。
type Entry struct {
	Path         string
	RelativePath string
	Name         string
	Size         uint64
	SeedHashHex  string
	BlockCount   uint64
	Seed         []byte
	State        string
	Error        string
}

// Failure 是一次实际文件访问或索引计算失败，不等价于文件已删除。
type Failure struct{ Path, Reason string }

// Report 区分新计算、复用和移除，不把缓存复用说成重新读取文件。
type Report struct {
	Indexed  int       `json:"indexed"`
	Reused   int       `json:"reused"`
	Removed  int       `json:"removed"`
	Failures []Failure `json:"failures"`
}

type mutation struct {
	ctx   context.Context
	apply func(*sql.Tx) error
	done  chan error
}

// Catalog 是单个售卖目录的唯一查询入口；只允许写队列访问写连接。
type Catalog struct {
	Root      string
	readers   *sql.DB
	writer    *sql.DB
	jobs      chan mutation
	finished  chan struct{}
	rootLock  *os.File
	dbLock    *os.File
	lifecycle sync.RWMutex
	closed    bool
	refreshMu sync.Mutex
	faultsMu  sync.RWMutex
	faults    map[string]string
}

// Open 打开磁盘索引并绑定根目录；不隐式遍历或计算文件哈希。
func Open(ctx context.Context, root, database string) (*Catalog, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("catalog: root is not a directory")
	}
	database, err = filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, database)
	if err != nil {
		return nil, err
	}
	if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("catalog: database must be outside the sale directory")
	}
	if err := os.MkdirAll(filepath.Dir(database), 0700); err != nil {
		return nil, err
	}
	c := &Catalog{Root: root, jobs: make(chan mutation, 64), finished: make(chan struct{}), faults: map[string]string{}}
	c.rootLock, err = os.Open(root)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Catalog, error) {
		if c.readers != nil {
			c.readers.Close()
		}
		if c.writer != nil {
			c.writer.Close()
		}
		if c.dbLock != nil {
			c.dbLock.Close()
		}
		c.rootLock.Close()
		return nil, err
	}
	if err := unix.Flock(int(c.rootLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("catalog: sale directory is already managed: %w", err))
	}
	c.dbLock, err = os.OpenFile(database+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	if err := unix.Flock(int(c.dbLock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("catalog: database is already managed: %w", err))
	}
	// 先限制主库权限；WAL/SHM 继承 SQLite 主文件的权限。
	file, err := os.OpenFile(database, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return fail(err)
	}
	file.Close()
	u := &url.URL{Scheme: "file", Path: database}
	c.writer, err = sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		return fail(err)
	}
	c.writer.SetMaxOpenConns(1)
	go c.writeLoop()
	// 包括 schema 和初始化在内的所有写入都排队，不存在第二条写路径。
	err = c.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS catalog_meta (id INTEGER PRIMARY KEY CHECK(id=1), root TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS mappings (path TEXT PRIMARY KEY, size INTEGER NOT NULL, seed_hash TEXT NOT NULL, blocks INTEGER NOT NULL, seed BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS mappings_seed ON mappings(seed_hash, path);`)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO catalog_meta(id,root) VALUES(1,?)", root); err != nil {
			return err
		}
		var saved string
		if err := tx.QueryRowContext(ctx, "SELECT root FROM catalog_meta WHERE id=1").Scan(&saved); err != nil {
			return err
		}
		if saved != root {
			return errors.New("catalog: database belongs to a different sale directory")
		}
		return nil
	})
	if err != nil {
		close(c.jobs)
		<-c.finished
		return fail(err)
	}
	c.readers, err = sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err == nil {
		c.readers.SetMaxOpenConns(4)
		err = c.readers.PingContext(ctx)
	}
	if err != nil {
		close(c.jobs)
		<-c.finished
		return fail(err)
	}
	return c, nil
}

// writeLoop 只执行短事务；大文件读取和哈希计算在事务外，提交时只核对文件属性。
func (c *Catalog) writeLoop() {
	defer close(c.finished)
	for job := range c.jobs {
		tx, err := c.writer.BeginTx(job.ctx, nil)
		if err == nil {
			err = job.apply(tx)
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
		job.done <- err
	}
}

func (c *Catalog) write(ctx context.Context, apply func(*sql.Tx) error) error {
	job := mutation{ctx: ctx, apply: apply, done: make(chan error, 1)}
	select {
	case c.jobs <- job:
	case <-ctx.Done():
		return ctx.Err()
	}
	// 已入队的操作必须明确完成后才返回，避免退出时留下不确定写入。
	return <-job.done
}

// Close 等待已进入的查询/刷新及写队列结束，再释放数据库和目录锁。
func (c *Catalog) Close() error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.jobs)
	<-c.finished
	return errors.Join(c.readers.Close(), c.writer.Close(), c.dbLock.Close(), c.rootLock.Close())
}

// walk 直接列磁盘；目录读取失败必须向上传播，不能清空数据库映射。
func (c *Catalog) walk(ctx context.Context) ([]*Entry, error) {
	entries := []*Entry{}
	err := filepath.WalkDir(c.Root, func(path string, item fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			if path != c.Root && strings.HasPrefix(item.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if item.Type()&os.ModeSymlink != 0 || strings.HasPrefix(item.Name(), ".") || strings.HasSuffix(item.Name(), ".km") {
			return nil
		}
		relative, err := filepath.Rel(c.Root, path)
		if err != nil {
			return err
		}
		entry := &Entry{Path: path, RelativePath: filepath.ToSlash(relative), Name: item.Name(), State: "unindexed"}
		info, err := item.Info()
		if err != nil {
			entry.State = "failed"
			entry.Error = err.Error()
			entries = append(entries, entry)
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return nil
		}
		entry.Size = uint64(info.Size())
		entries = append(entries, entry)
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelativePath < entries[j].RelativePath })
	return entries, err
}

// List 每次列文件系统，再关联已索引元数据；不计算哈希，不读取所有 Seed。
func (c *Catalog) List(ctx context.Context) ([]*Entry, error) {
	c.lifecycle.RLock()
	defer c.lifecycle.RUnlock()
	if c.closed {
		return nil, errors.New("catalog: closed")
	}
	entries, err := c.walk(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, entry := range entries {
		if entry.Error != "" {
			continue
		}
		err := tx.QueryRowContext(ctx, "SELECT seed_hash,blocks FROM mappings WHERE path=? AND size=?", entry.RelativePath, entry.Size).Scan(&entry.SeedHashHex, &entry.BlockCount)
		if err == nil {
			entry.State = "ready"
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		c.faultsMu.RLock()
		fault := c.faults[entry.RelativePath]
		c.faultsMu.RUnlock()
		if fault != "" {
			entry.State = "failed"
			entry.Error = fault
		}
	}
	return entries, tx.Commit()
}

// Preferred 按哈希查数据库并检查磁盘路径和长度；不会重算已有文件哈希。
func (c *Catalog) Preferred(ctx context.Context, hash string) (*Entry, error) {
	c.lifecycle.RLock()
	defer c.lifecycle.RUnlock()
	if c.closed {
		return nil, errors.New("catalog: closed")
	}
	rows, err := c.readers.QueryContext(ctx, "SELECT path,size,seed_hash,blocks,seed FROM mappings WHERE seed_hash=? ORDER BY path", hash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		entry := &Entry{State: "ready"}
		if err := rows.Scan(&entry.RelativePath, &entry.Size, &entry.SeedHashHex, &entry.BlockCount, &entry.Seed); err != nil {
			return nil, err
		}
		entry.Path = filepath.Join(c.Root, filepath.FromSlash(entry.RelativePath))
		entry.Name = filepath.Base(entry.Path)
		c.faultsMu.RLock()
		fault := c.faults[entry.RelativePath]
		c.faultsMu.RUnlock()
		if fault != "" {
			continue
		}
		info, err := os.Lstat(entry.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || uint64(info.Size()) != entry.Size {
			continue
		}
		return entry, nil
	}
	return nil, rows.Err()
}

// Refresh 只以路径和长度判断变化；force 是用户显式要求重算所有内容。
func (c *Catalog) Refresh(ctx context.Context, force bool) (Report, error) {
	c.lifecycle.RLock()
	defer c.lifecycle.RUnlock()
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	report := Report{Failures: []Failure{}}
	if c.closed {
		return report, errors.New("catalog: closed")
	}
	entries, err := c.walk(ctx)
	if err != nil {
		return report, err
	}
	seen := map[string]bool{}
	faults := map[string]string{}
	defer func() { c.faultsMu.Lock(); c.faults = faults; c.faultsMu.Unlock() }()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		seen[entry.RelativePath] = true
		var savedSize uint64
		err := c.readers.QueryRowContext(ctx, "SELECT size FROM mappings WHERE path=?", entry.RelativePath).Scan(&savedSize)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return report, err
		}
		if entry.Error != "" {
			faults[entry.RelativePath] = entry.Error
			report.Failures = append(report.Failures, Failure{entry.RelativePath, entry.Error})
			continue
		}
		if !force && err == nil && savedSize == entry.Size {
			report.Reused++
			continue
		}
		// 先撤下旧身份，再计算；计算失败不能继续发布旧映射。
		if err := c.write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "DELETE FROM mappings WHERE path=?", entry.RelativePath)
			return err
		}); err != nil {
			return report, err
		}
		if err := indexFile(ctx, entry); err != nil {
			faults[entry.RelativePath] = err.Error()
			report.Failures = append(report.Failures, Failure{entry.RelativePath, err.Error()})
			continue
		}
		err = c.write(ctx, func(tx *sql.Tx) error {
			info, err := os.Lstat(entry.Path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || uint64(info.Size()) != entry.Size {
				return errors.New("catalog: source length changed before publishing")
			}
			_, err = tx.ExecContext(ctx, "INSERT INTO mappings(path,size,seed_hash,blocks,seed) VALUES(?,?,?,?,?)", entry.RelativePath, entry.Size, entry.SeedHashHex, entry.BlockCount, entry.Seed)
			return err
		})
		if err != nil {
			return report, err
		}
		report.Indexed++
	}
	// 仅完整列目录成功后清理消失路径，不据数据库构建文件树。
	err = c.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT path FROM mappings")
		if err != nil {
			return err
		}
		removed := []string{}
		for rows.Next() {
			var path string
			if err := rows.Scan(&path); err != nil {
				rows.Close()
				return err
			}
			if !seen[path] {
				removed = append(removed, path)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, path := range removed {
			if _, err := tx.ExecContext(ctx, "DELETE FROM mappings WHERE path=?", path); err != nil {
				return err
			}
		}
		report.Removed = len(removed)
		return nil
	})
	return report, err
}

// indexFile 只处理新增、长度变化或显式重算的文件；不以 mtime 判断变化。
func indexFile(ctx context.Context, entry *Entry) error {
	file, err := os.Open(entry.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	var seed bytes.Buffer
	info, err := masterseed.CreateSeed(ctx, file, &seed)
	if err != nil {
		return err
	}
	current, err := file.Stat()
	if err != nil {
		return err
	}
	if uint64(current.Size()) != entry.Size {
		return errors.New("catalog: source length changed while indexing")
	}
	entry.SeedHashHex = info.SeedHashHex
	entry.BlockCount = info.BlockCount
	entry.Seed = seed.Bytes()
	return nil
}

// ReadBlock 从实际文件读取一块并验证交付哈希，不重新计算整文件身份。
func (c *Catalog) ReadBlock(ctx context.Context, entry *Entry, hash []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for index := uint64(0); index < entry.BlockCount; index++ {
		expected, err := masterseed.ReadBlockHash(ctx, bytes.NewReader(entry.Seed), uint64(len(entry.Seed)), index)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(expected.Bytes(), hash) {
			continue
		}
		size, err := masterseed.ExpectedBlockSize(entry.Size, index)
		if err != nil {
			return nil, err
		}
		file, err := os.Open(entry.Path)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if uint64(info.Size()) != entry.Size {
			return nil, errors.New("catalog: source length changed")
		}
		buffer := make([]byte, size)
		if _, err := file.ReadAt(buffer, int64(index)*masterseed.BlockSize); err != nil {
			return nil, err
		}
		actual := sha256.Sum256(buffer)
		if !bytes.Equal(actual[:], hash) {
			return nil, errors.New("catalog: block content does not match requested hash")
		}
		return buffer, nil
	}
	return nil, fmt.Errorf("catalog: block %x is not part of seed %s", hash, entry.SeedHashHex)
}
