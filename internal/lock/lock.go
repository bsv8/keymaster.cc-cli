// Package lock provides single-flight for one download scope: at most one
// live holder per lock file. The lock is advisory and process-scoped by the
// kernel, but enforced here by the OS so two processes (or two runs in one
// process) cannot drive the same seed download concurrently. Releasing is
// implicit on process exit; a stale lock therefore always means a live holder
// on a working system, never a leftover to clean up.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrActive reports that the scope already has a live holder.
var ErrActive = errors.New("lock: scope already has an active holder")

// Guard holds one acquired lock until Release.
type Guard struct {
	file *os.File
	path string
}

// Acquire takes an exclusive, non-blocking lock on path, creating parent
// directories and the file as needed. The caller must hold the Guard for as
// long as the scoped work runs.
func Acquire(path string) (*Guard, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("lock: create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open lock file: %w", err)
	}
	if err := lockExclusiveNonblock(file); err != nil {
		_ = file.Close()
		return nil, err
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
