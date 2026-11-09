package appconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateNewRoot checks a candidate root path for sharing. dataDir is the km
// data directory; internal wallet, config and business data must never become
// shared roots. Symlinks are resolved to their canonical target first.
func ValidateNewRoot(dataDir, candidate string, existing []Root) (string, error) {
	if strings.TrimSpace(candidate) == "" {
		return "", errors.New("售卖目录不能为空")
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("解析售卖目录失败: %w", err)
	}
	// Resolve the root itself so /tmp/link and /real/target compare equal.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("售卖目录不可访问: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("售卖目录不是目录")
	}
	cleaned := filepath.Clean(abs)
	dataAbs, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	dataAbs = filepath.Clean(dataAbs)
	// Internal data directories must never be shared.
	if cleaned == dataAbs || isWithin(cleaned, dataAbs) || isWithin(dataAbs, cleaned) {
		return "", errors.New("内部数据目录不能作为共享根")
	}
	for _, r := range existing {
		other := filepath.Clean(r.Path)
		if cleaned == other {
			return "", errors.New("售卖目录重复")
		}
		if isWithin(cleaned, other) || isWithin(other, cleaned) {
			return "", errors.New("售卖目录存在父子重叠")
		}
	}
	return cleaned, nil
}

func isWithin(child, parent string) bool {
	if child == parent {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// SafeJoin joins root with a slash-separated relative path and guarantees the
// result stays inside root. It rejects absolute paths, parent escapes and
// symlink escapes (resolved target must remain inside the root).
func SafeJoin(root, relSlash string) (string, error) {
	if filepath.IsAbs(relSlash) || filepath.IsAbs(filepath.FromSlash(relSlash)) {
		return "", errors.New("相对路径不能是绝对路径")
	}
	cleaned := filepath.Clean(filepath.FromSlash(relSlash))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("相对路径越界")
	}
	target := filepath.Join(root, cleaned)
	// If the target (or its parent chain) contains symlinks that escape the
	// root, the resolved path will leave the root: reject it.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = filepath.Clean(root)
	}
	parent := target
	// Resolve as far as possible: if the file does not exist yet, resolve the
	// nearest existing ancestor and re-anchor the remainder.
	for {
		if _, err := os.Lstat(parent); err == nil {
			resolved, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", fmt.Errorf("解析符号链接失败: %w", err)
			}
			rel, err := filepath.Rel(resolvedRoot, resolved)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", errors.New("符号链接越过根目录边界")
			}
			if parent == target {
				return target, nil
			}
			rest, err := filepath.Rel(parent, target)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, rest), nil
		}
		next := filepath.Dir(parent)
		if next == parent {
			// Nothing exists yet (fresh download target): anchor on root.
			rel, err := filepath.Rel(resolvedRoot, target)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", errors.New("符号链接越过根目录边界")
			}
			return target, nil
		}
		parent = next
	}
}
