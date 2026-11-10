//go:build !windows

package daemonrun

import (
	"os"
	"os/signal"
	"syscall"
)

// sighupChannel reports SIGHUP for orderly restart (headless restart and
// E2E). A buffered channel avoids missing the signal during supervision.
func sighupChannel() <-chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	return ch
}
