package appstorage

const (
	// DirectoryMarkerSuffix 与 cc 一致：目录只是空对象 marker，不额外占用文件。
	DirectoryMarkerSuffix = "/.dir"
	// DirectoryMIME 标记该对象是目录占位，列表时隐藏。
	DirectoryMIME = "application/x-directory"
)

// CreateDirectory 创建空目录 marker；overwrite=false 时依赖唯一约束条件创建。
func (s *Store) CreateDirectory(network, wallet, app, path string, overwrite bool) (rev, updatedAt int64, err error) {
	clean, err := validatePath(path)
	if err != nil {
		return 0, 0, err
	}
	ifNone := ""
	if !overwrite {
		ifNone = "*"
	}
	return s.Put(network, wallet, app, clean+DirectoryMarkerSuffix, []byte{}, DirectoryMIME, ifNone, 0)
}

// DeleteDirectory 只允许删除只含自身 marker 的空目录；有文件或子目录时拒绝。
// 与写队列共用锁，避免检查后又有写入把目录变成非空。
func (s *Store) DeleteDirectory(network, wallet, app, path string) error {
	clean, err := validatePath(path)
	if err != nil {
		return err
	}
	marker := clean + DirectoryMarkerSuffix
	prefix := clean + "/"
	if err := s.Begin(); err != nil {
		return err
	}
	defer s.End()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// 与 List 共用字面前缀规则；LIKE 的 _ / % 会误匹配合法目录名。
	rows, err := s.db.Query(`SELECT path FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND substr(path,1,length(?))=? LIMIT ?`,
		network, wallet, app, prefix, prefix, MaxListLimit+1)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			rows.Close()
			return err
		}
		count++
		if count > MaxListLimit {
			rows.Close()
			return ErrNotEmpty
		}
		// 任何非自身 marker 的对象（文件或子目录 marker）都表示非空。
		if entry != marker {
			rows.Close()
			return ErrNotEmpty
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	res, err := s.db.Exec(`DELETE FROM objects WHERE network=? AND wallet_pubkey=? AND app_pubkey=? AND path=?`,
		network, wallet, app, marker)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
