//go:build linux

package tray

// hasTrayHost reports whether a StatusNotifier host is reachable on the
// session bus. Absence is normal on headless machines and plain window
// managers; the caller must degrade to headless operation, not fail.
func hasTrayHost() (bool, string) {
	names, err := sessionBusNames(3000)
	if err != nil {
		return false, "无会话总线: " + err.Error()
	}
	for _, want := range []string{
		"org.kde.StatusNotifierWatcher",
		"org.freedesktop.StatusNotifierWatcher",
	} {
		for _, n := range names {
			if n == want {
				return true, ""
			}
		}
	}
	return false, "未发现托盘宿主（需要支持 StatusNotifier 的桌面环境）"
}
