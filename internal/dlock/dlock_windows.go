//go:build windows

package dlock

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockExclusiveNonblock(file *os.File) error {
	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		return ErrActive
	}
	return nil
}
