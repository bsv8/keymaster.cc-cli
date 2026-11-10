//go:build windows

package daemonrun

import "os"

// sighupChannel has no source on Windows; restarts arrive via the tray
// menu or service control instead. A nil channel blocks forever.
func sighupChannel() <-chan os.Signal { return nil }
