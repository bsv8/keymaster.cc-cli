//go:build unix

package dlock

import (
	"os"
	"syscall"
)

func lockExclusiveNonblock(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrActive
	}
	return nil
}
