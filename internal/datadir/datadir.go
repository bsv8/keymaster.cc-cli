// Package datadir resolves the km data directory layout and enforces the
// single-daemon file lock.
package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Layout is the resolved on-disk layout for one data directory.
type Layout struct {
	Dir          string
	KeyPath      string
	InfoPath     string
	SettingsPath string
	LegacyConfig string
	ClientDir    string
	ServerDir    string
	RootsDir     string
	SessionsDir  string
	LockPath     string
}

// Resolve returns the layout for dir. Empty dir means default (~/.km).
func Resolve(dir string) (Layout, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Layout{}, fmt.Errorf("datadir: 无法定位用户主目录: %w", err)
		}
		dir = filepath.Join(home, ".km")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Layout{}, fmt.Errorf("datadir: 解析数据目录失败: %w", err)
	}
	return Layout{
		Dir:          abs,
		KeyPath:      filepath.Join(abs, "key.json"),
		InfoPath:     filepath.Join(abs, "info.json"),
		SettingsPath: filepath.Join(abs, "settings.json"),
		LegacyConfig: filepath.Join(abs, "config.json"),
		ClientDir:    filepath.Join(abs, "client"),
		ServerDir:    filepath.Join(abs, "server"),
		RootsDir:     filepath.Join(abs, "server", "roots"),
		SessionsDir:  filepath.Join(abs, "server", "sessions"),
		LockPath:     filepath.Join(abs, "lock"),
	}, nil
}

// EnsureDirs creates the data directory skeleton with restrictive perms.
func (l Layout) EnsureDirs() error {
	for _, dir := range []string{l.Dir, l.ClientDir, l.ServerDir, l.RootsDir, l.SessionsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("datadir: 创建目录失败 %s: %w", dir, err)
		}
	}
	return nil
}

// HasInitMarkers reports whether the new-layout init markers exist.
func (l Layout) HasInitMarkers() bool {
	if _, err := os.Stat(l.KeyPath); err != nil {
		return false
	}
	if _, err := os.Stat(l.InfoPath); err != nil {
		return false
	}
	if _, err := os.Stat(l.SettingsPath); err != nil {
		return false
	}
	return true
}

// HasAnyWallet reports whether any wallet material exists (new or legacy).
// Corrupt or half-written files still count as "exists" so callers never
// mistake damage for a fresh install.
func (l Layout) HasAnyWallet() bool {
	for _, p := range []string{l.KeyPath, l.InfoPath, l.SettingsPath, l.LegacyConfig} {
		if _, statErr := os.Stat(p); statErr == nil {
			return true
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return true
		}
	}
	return false
}
