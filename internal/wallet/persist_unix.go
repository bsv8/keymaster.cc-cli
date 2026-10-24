// Package wallet file helpers: strict reads, atomic durable writes and the
// single-writer directory lock.
package wallet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// readFileStrict reads a state file: missing file reports (nil, nil); any
// other I/O error (permissions, I/O failure) is a hard error and must never
// be mistaken for "no history".
func readFileStrict(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("wallet: read %s: %w", path, err)
	}
	return blob, nil
}

// atomicWriteFile writes through a temp file, fsyncs it, renames over the
// target and syncs the directory, so a crash cannot leave a half-written
// manager state behind.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manager-*.tmp")
	if err != nil {
		return fmt.Errorf("wallet: temp state file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("wallet: write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("wallet: fsync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wallet: close state: %w", err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("wallet: chmod state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("wallet: commit state: %w", err)
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("wallet: open state dir: %w", err)
	}
	defer func() { _ = dirFile.Close() }()
	if err := dirFile.Sync(); err != nil {
		return fmt.Errorf("wallet: fsync state dir: %w", err)
	}
	return nil
}
