//go:build windows

package lock

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockExclusiveNonblock(file *os.File) error {
	// Lock a single byte; the range is arbitrary, only exclusivity matters.
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped)
	if err != nil {
		return ErrActive
	}
	return nil
}
