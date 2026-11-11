package appstorage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Object 为 App 对象行。
type Object struct {
	Network  string
	Wallet   string
	App      string
	Path     string
	Content  []byte
	MIME     string
	Revision int64
	Updated  int64
	Total    int64
}

// MaxStoredObjectBytes 是数据库允许的存储字节上限。Connect 明文上限为
// 16 MiB；加密信封使用十六进制密文，体积约两倍，因此存储层必须单独放宽。
const MaxStoredObjectBytes = 36 << 20

var (
	ErrNotFound      = errors.New("对象不存在")
	ErrConflict      = errors.New("条件写冲突")
	ErrTooLarge      = errors.New("对象过大")
	ErrInvalidPath   = errors.New("路径非法")
	ErrInvalidCursor = errors.New("游标非法")
	ErrNotEmpty      = errors.New("目录非空")
)

// validatePath 校验相对路径：禁止绝对、..、空段。
func validatePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("%w: 路径为空", ErrInvalidPath)
	}
	if strings.HasPrefix(p, "/") {
		p = strings.TrimPrefix(p, "/")
	}
	if p == "" || p == "." {
		return "", fmt.Errorf("%w: 路径为空", ErrInvalidPath)
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: %q", ErrInvalidPath, p)
		}
	}
	return strings.Join(parts, "/"), nil
}

// Put 原子提交内容、MIME、revision 和时间；条件创建走唯一约束+事务，不先读后写。
// ifNoneMatch 为 "*" 时仅当不存在才创建；revisionMismatch（期望 revision）用于条件更新，0 表示不检查。
func (s *Store) Put(network, wallet, app, path string, content []byte, mime string, ifNoneMatch string, expectRevision int64) (rev int64, updatedAt int64, err error) {
	clean, err := validatePath(path)
	if err != nil {
		return 0, 0, err
	}
	if content == nil {
		content = []byte{}
	}
	if len(content) > MaxStoredObjectBytes {
		return 0, 0, ErrTooLarge
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	if err := s.Begin(); err != nil {
		return 0, 0, err
	}
	defer s.End()
	// 串行化写事务：延迟事务先读后写时，并发写会直接返回 SQLITE_BUSY，
	// busy_timeout 对死锁式升级无效。所有写路径共用这一把锁。
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := time.Now().UnixMilli()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var curRev int64
	var exists bool
	err = tx.QueryRow(`SELECT revision FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path=?`, network, wallet, app, clean).Scan(&curRev)
	if err == nil {
		exists = true
	} else if errors.Is(err, sql.ErrNoRows) {
		exists = false
	} else {
		return 0, 0, err
	}
	if ifNoneMatch == "*" && exists {
		return 0, 0, ErrConflict
	}
	if exists {
		if expectRevision != 0 && expectRevision != curRev {
			return 0, 0, ErrConflict
		}
	} else if expectRevision != 0 {
		return 0, 0, ErrConflict
	}
	// revision 取全局单调序列，删除后重建也不会复用旧版本标签；更新先把
	// 计数器写入以获得 SQLite 写锁，避免并发事务分配到同一版本。
	if _, err := tx.Exec(`UPDATE meta SET value = value + 1 WHERE key='revision_seq'`); err != nil {
		return 0, 0, err
	}
	var nextRev int64
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='revision_seq'`).Scan(&nextRev); err != nil {
		return 0, 0, err
	}
	if exists {
		_, err = tx.Exec(`UPDATE objects SET content=?, mime=?, revision=?, updated_at=? WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path=?`,
			content, mime, nextRev, now, network, wallet, app, clean)
	} else {
		_, err = tx.Exec(`INSERT INTO objects(network,wallet_pubkey,app_pubkey,path,content,mime,revision,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			network, wallet, app, clean, content, mime, nextRev, now)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY") {
				return 0, 0, ErrConflict
			}
			return 0, 0, err
		}
	}
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY") {
			return 0, 0, ErrConflict
		}
		return 0, 0, err
	}
	return nextRev, now, nil
}

// Get 读取对象；offset/length 用于范围读取（length<0 表示到末尾）。
func (s *Store) Get(network, wallet, app, path string, offset, length int64) (Object, error) {
	clean, err := validatePath(path)
	if err != nil {
		return Object{}, err
	}
	if err := s.Begin(); err != nil {
		return Object{}, err
	}
	defer s.End()
	var o Object
	var content []byte
	err = s.db.QueryRow(`SELECT network,wallet_pubkey,app_pubkey,path,content,mime,revision,updated_at,length(content) FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path=?`,
		network, wallet, app, clean).Scan(&o.Network, &o.Wallet, &o.App, &o.Path, &content, &o.MIME, &o.Revision, &o.Updated, &o.Total)
	if errors.Is(err, sql.ErrNoRows) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(content)) {
		return Object{}, fmt.Errorf("%w: 范围越界", ErrInvalidPath)
	}
	end := int64(len(content))
	if length >= 0 && length < end-offset {
		end = offset + length
	}
	o.Content = content[offset:end]
	return o, nil
}

// Delete 删除对象；不存在返回 NotFound。
func (s *Store) Delete(network, wallet, app, path string) error {
	clean, err := validatePath(path)
	if err != nil {
		return err
	}
	if err := s.Begin(); err != nil {
		return err
	}
	defer s.End()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.Exec(`DELETE FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path=?`, network, wallet, app, clean)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DefaultListLimit 与 MaxListLimit 与 Connect SDK 的
// STORAGE_DEFAULT_LIST_LIMIT / STORAGE_MAX_LIST_LIMIT 一致，
// 由 parity/KeymasterConnect 跨语言向量锁定。
const (
	DefaultListLimit = 200
	MaxListLimit     = 1000
)

// Entry 为列举条目（不含内容）。
type Entry struct {
	Path     string `json:"path"`
	MIME     string `json:"mime"`
	Revision int64  `json:"revision"`
	Size     int64  `json:"size"`
	Updated  int64  `json:"updatedAt"`
}

// List 按前缀列举，游标绑定查询上下文；limit<=0 默认 DefaultListLimit
// （与 Connect SDK STORAGE_DEFAULT_LIST_LIMIT 一致，跨语言向量锁定），
// 上限 MaxListLimit（STORAGE_MAX_LIST_LIMIT）。
func (s *Store) List(network, wallet, app, prefix, cursor string, limit int) (entries []Entry, nextCursor string, err error) {
	prefix = strings.TrimPrefix(strings.TrimSpace(prefix), "/")
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	if err := s.Begin(); err != nil {
		return nil, "", err
	}
	defer s.End()
	// 游标为上次最后 path；绑定前缀+授权上下文，换前缀或换授权则游标无效。
	// 下一页从游标之后开始（严格大于），避免重复返回上一页最后一条。
	if cursor != "" {
		if prefix != "" && !strings.HasPrefix(cursor, prefix) {
			return nil, "", ErrInvalidCursor
		}
		if _, err := validatePath(cursor); err != nil {
			return nil, "", ErrInvalidCursor
		}
	}
	// 前缀用 substr 按字面、区分大小写比较；LIKE 会把 _ / % 当通配符。
	query := `SELECT path,mime,revision,length(content),updated_at FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path>? ORDER BY path LIMIT ?`
	args := []any{network, wallet, app, cursor, limit + 1}
	if prefix != "" {
		query = `SELECT path,mime,revision,length(content),updated_at FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path>? AND substr(path,1,length(?))=? ORDER BY path LIMIT ?`
		args = []any{network, wallet, app, cursor, prefix, prefix, limit + 1}
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var e Entry
		var size int64
		if err := rows.Scan(&e.Path, &e.MIME, &e.Revision, &size, &e.Updated); err != nil {
			return nil, "", err
		}
		e.Size = size
		entries = append(entries, e)
	}
	if len(entries) > limit {
		entries = entries[:limit]
		// nextCursor 为最后返回条目的 path，下次从其后开始。
		nextCursor = entries[len(entries)-1].Path
	} else {
		nextCursor = ""
	}
	return entries, nextCursor, rows.Err()
}
