//go:build !linux

package tray

// hasTrayHost always attempts the native tray on non-Linux platforms;
// macOS uses the menu bar and Windows the notification area, neither of
// which needs a separate host process.
func hasTrayHost() (bool, string) {
	return true, ""
}
