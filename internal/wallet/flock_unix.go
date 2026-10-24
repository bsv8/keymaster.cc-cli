//go:build unix

package wallet

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// flockExclusive takes an exclusive, non-blocking lock for the wallet
// directory. A second process holding the same directory fails fast.
func flockExclusive(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("wallet: lock directory: %w", err)
	}
	return nil
}

// flockRelease releases the directory lock.
func flockRelease(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
