// Package store implements the per-seedHash client working store.
//
// Two write modes exist:
//   - SaveExact: first write wins; the same name can never be bound to a
//     different byte sequence (protocol artifacts and raw transactions).
//   - Save: atomic replace for state snapshots that are expected to change.
package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store is a rooted directory with 0700 directories and 0600 files.
type Store struct {
	root string
}

// Open creates (if needed) and returns the store rooted at root.
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("store: root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("store: create root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the store root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) path(name string) (string, error) {
	cleaned := filepath.Clean(name)
	if cleaned == "." || cleaned == "" || filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return "", fmt.Errorf("store: invalid name %q", name)
	}
	return filepath.Join(s.root, cleaned), nil
}

// SaveExact writes bytes under name. When name already exists it must hold the
// same bytes; a conflicting byte sequence is rejected.
func (s *Store) SaveExact(name string, data []byte) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return fmt.Errorf("store: refusing to write empty %s", name)
	}
	if existing, readErr := os.ReadFile(path); readErr == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("store: %s is already bound to different bytes", name)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("store: create directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".exact-*.tmp")
	if err != nil {
		return fmt.Errorf("store: create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("store: write %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Link is atomic and fails when the target exists, so a concurrent writer
	// can never replace already-visible exact bytes.
	if err := os.Link(temporaryName, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			existing, readErr := os.ReadFile(path)
			if readErr == nil && bytes.Equal(existing, data) {
				return nil
			}
			return fmt.Errorf("store: %s is already bound to different bytes", name)
		}
		return fmt.Errorf("store: publish %s: %w", name, err)
	}
	return nil
}

// Save replaces name atomically (state snapshots only).
func (s *Store) Save(name string, data []byte) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("store: create directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".save-*.tmp")
	if err != nil {
		return fmt.Errorf("store: create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("store: write %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("store: replace %s: %w", name, err)
	}
	return nil
}

// Load reads one entry.
func (s *Store) Load(name string) ([]byte, error) {
	path, err := s.path(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Exists reports whether name exists.
func (s *Store) Exists(name string) (bool, error) {
	path, err := s.path(name)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(path)
	if statErr == nil {
		return true, nil
	}
	if errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	return false, statErr
}

// List returns the relative names under prefix, sorted.
func (s *Store) List(prefix string) ([]string, error) {
	base := s.root
	if prefix != "" {
		cleaned := filepath.Clean(prefix)
		if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || filepath.IsAbs(cleaned) {
			return nil, fmt.Errorf("store: invalid prefix %q", prefix)
		}
		base = filepath.Join(s.root, cleaned)
	}
	names := make([]string, 0)
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, relErr := filepath.Rel(s.root, path)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(filepath.Base(relative), ".exact-") || strings.HasPrefix(filepath.Base(relative), ".save-") {
			return nil
		}
		names = append(names, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("store: list %q: %w", prefix, err)
	}
	sort.Strings(names)
	return names, nil
}

// Remove deletes a store entry. Removing a missing entry is a no-op.
func (s *Store) Remove(name string) error {
	path, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: remove %s: %w", name, err)
	}
	return nil
}
