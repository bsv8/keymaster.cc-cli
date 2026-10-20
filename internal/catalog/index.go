// Package catalog indexes the files a km server offers for sale.
//
// The index is derived from the filesystem: each file is hashed with MasterSeed
// (256 KiB blocks, seed = concatenated block hashes). Content identity is the
// seed hash; a file modification therefore produces a new identity and the old
// content is no longer advertised.
package catalog

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	masterseed "github.com/bsv8/MasterSeed"
)

// Entry is one indexed file.
type Entry struct {
	// Path is the absolute file path.
	Path string
	// RelativePath is the slash-separated path relative to the server root.
	RelativePath string
	// Name is the base name.
	Name string
	// Size is the source size in bytes.
	Size uint64
	// ModTime is the source modification time.
	ModTime time.Time
	// SeedHashHex is the hex seed hash (content identity).
	SeedHashHex string
	// BlockCount is the number of 256 KiB blocks.
	BlockCount uint64
	// Seed holds the raw seed bytes (block hashes).
	Seed []byte
}

// Failure describes one file that could not be indexed.
type Failure struct {
	Path   string
	Reason string
}

// Index is the result of a scan.
type Index struct {
	Root     string
	Entries  []*Entry
	BySeed   map[string][]*Entry
	Failures []Failure
}

// Scan walks root and indexes every sellable file.
func Scan(ctx context.Context, root string) (*Index, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("catalog: resolve root: %w", err)
	}
	info, err := os.Stat(absoluteRoot)
	if err != nil {
		return nil, fmt.Errorf("catalog: root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("catalog: root is not a directory: %s", absoluteRoot)
	}
	index := &Index{Root: absoluteRoot, Entries: []*Entry{}, BySeed: map[string][]*Entry{}}
	err = filepath.WalkDir(absoluteRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			index.Failures = append(index.Failures, Failure{Path: path, Reason: walkErr.Error()})
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != absoluteRoot && strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".km") {
			return nil
		}
		fileInfo, infoErr := entry.Info()
		if infoErr != nil {
			index.Failures = append(index.Failures, Failure{Path: path, Reason: infoErr.Error()})
			return nil
		}
		if fileInfo.Size() == 0 {
			return nil
		}
		indexed, indexErr := indexFile(ctx, absoluteRoot, path, fileInfo)
		if indexErr != nil {
			index.Failures = append(index.Failures, Failure{Path: path, Reason: indexErr.Error()})
			return nil
		}
		index.Entries = append(index.Entries, indexed)
		index.BySeed[indexed.SeedHashHex] = append(index.BySeed[indexed.SeedHashHex], indexed)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: scan: %w", err)
	}
	sort.Slice(index.Entries, func(i, j int) bool { return index.Entries[i].RelativePath < index.Entries[j].RelativePath })
	return index, nil
}

func indexFile(ctx context.Context, root, path string, info os.FileInfo) (*Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var seedBuffer bytes.Buffer
	seedInfo, err := masterseed.CreateSeed(ctx, file, &seedBuffer)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	return &Entry{
		Path:         path,
		RelativePath: filepath.ToSlash(relative),
		Name:         info.Name(),
		Size:         uint64(info.Size()),
		ModTime:      info.ModTime().UTC(),
		SeedHashHex:  seedInfo.SeedHashHex,
		BlockCount:   seedInfo.BlockCount,
		Seed:         append([]byte(nil), seedBuffer.Bytes()...),
	}, nil
}

// Find returns the entries with the given seed hash.
func (i *Index) Find(seedHashHex string) []*Entry { return i.BySeed[seedHashHex] }

// Preferred returns the lexicographically first entry for a seed hash, which is
// the path offered when one content identity appears under several paths.
func (i *Index) Preferred(seedHashHex string) *Entry {
	entries := i.BySeed[seedHashHex]
	if len(entries) == 0 {
		return nil
	}
	best := entries[0]
	for _, entry := range entries[1:] {
		if entry.RelativePath < best.RelativePath {
			best = entry
		}
	}
	return best
}
