//go:build unix

package catalog

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockExclusiveNB takes an advisory exclusive non-blocking lock on an open
// file. A stale lock always means a live manager on a working system.
func lockExclusiveNB(file *os.File, what string) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("catalog: %s is already managed: %w", what, err)
	}
	return nil
}
