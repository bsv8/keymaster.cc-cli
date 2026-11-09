//go:build windows

package catalog

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusiveNB takes an advisory exclusive non-blocking lock on an open
// file. A stale lock always means a live manager on a working system.
func lockExclusiveNB(file *os.File, what string) error {
	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped); err != nil {
		return fmt.Errorf("catalog: %s is already managed: %w", what, err)
	}
	return nil
}
