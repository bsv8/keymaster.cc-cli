// Package dlock provides the single-daemon data directory lock: at most one
// live daemon per data directory. The lock is advisory and released on
// process exit; a stale lock always means a live holder.
package dlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrActive reports that another daemon already holds the data directory.
var ErrActive = errors.New("dlock: 数据目录已有后台在运行")

// Guard holds one acquired data directory lock until Release.
type Guard struct {
	file *os.File
	path string
}

// Acquire takes an exclusive, non-blocking lock on dir/lock.
func Acquire(dataDir string) (*Guard, error) {
	if dataDir == "" {
		return nil, errors.New("dlock: 数据目录不能为空")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("dlock: 创建数据目录失败: %w", err)
	}
	path := filepath.Join(dataDir, "lock")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dlock: 打开锁文件失败: %w", err)
	}
	if err := lockExclusiveNonblock(file); err != nil {
		_ = file.Close()
		return nil, ErrActive
	}
	return &Guard{file: file, path: path}, nil
}

// Release drops the lock. It is idempotent.
func (g *Guard) Release() error {
	if g == nil || g.file == nil {
		return nil
	}
	err := g.file.Close()
	g.file = nil
	return err
}
