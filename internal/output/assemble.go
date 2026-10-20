// Package output assembles verified staged content into the final download
// file.
//
// Assembly is seed-driven: the ordered block hashes, block sizes and total
// size all come from the verified MasterSeed, so no external manifest can
// smuggle extra or reordered content into the output. The in-progress file is
// <seed-hash>.km; finalization is an atomic rename to a sanitized unique name,
// guarded by a persisted pending record so an interrupted rename can always be
// retried and user data is never overwritten.
package output

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	masterseed "github.com/bsv8/MasterSeed"

	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

const (
	// TempFileSuffix marks an incomplete download.
	TempFileSuffix = ".km"
	// SeedStagingPath is where a verified seed is staged by the buyer.
	SeedStagingPath  = "staging/seed"
	pendingPath      = "output/pending.json"
	finalPath        = "output/final.json"
	renameIntentPath = "output/rename.json"
	maxNameBytes     = 255
)

// ErrIncomplete reports that not every block of the seed has been staged yet.
var ErrIncomplete = errors.New("output: content is not complete yet")

// PendingRename pins the final name chosen for an assembled file before the
// rename happens.
type PendingRename struct {
	SeedHashHex string `json:"seedHash"`
	FinalName   string `json:"finalName"`
	TotalBytes  int64  `json:"totalBytes"`
}

// FinalFile records the finished download for recovery and display.
type FinalFile struct {
	SeedHashHex string `json:"seedHash"`
	Path        string `json:"path"`
	Bytes       int64  `json:"bytes"`
}

// Result is one completed assembly.
type Result struct {
	Path  string
	Bytes int64
	// Reused reports that an earlier run already assembled and named the file.
	Reused bool
}

// Assembler finalizes downloads for one task store into one directory.
type Assembler struct {
	store *store.Store
	dir   string
}

// New creates an assembler; the output directory is created when missing.
func New(s *store.Store, outputDir string) (*Assembler, error) {
	if s == nil {
		return nil, errors.New("output: store is required")
	}
	if strings.TrimSpace(outputDir) == "" {
		return nil, errors.New("output: directory is required")
	}
	dir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, fmt.Errorf("output: resolve directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("output: create directory: %w", err)
	}
	return &Assembler{store: s, dir: dir}, nil
}

// Dir returns the assembly directory.
func (a *Assembler) Dir() string { return a.dir }

// renameIntent records a completed link before the final record is updated.
type renameIntent struct {
	SeedHashHex string `json:"seedHash"`
	From        string `json:"from"`
	To          string `json:"to"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// Final reads the recorded final file, if any. A pending rename intent is
// recovered first so a crash between the link and the final record update does
// not leave a stale path.
func (a *Assembler) Final() (*FinalFile, error) {
	if err := a.recoverRename(); err != nil {
		return nil, err
	}
	raw, err := a.store.Load(finalPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var final FinalFile
	if err := json.Unmarshal(raw, &final); err != nil {
		return nil, fmt.Errorf("output: final record is corrupt: %w", err)
	}
	return &final, nil
}

// Complete assembles the staged content for seedHashHex and finalizes it.
//
// It is idempotent: once the file is named, later calls return the same path
// with Reused set. A missing or corrupt block is an error (ErrIncomplete for
// missing), and the final name is never overwritten.
func (a *Assembler) Complete(ctx context.Context, seedHashHex, preferredName string) (*Result, error) {
	if !isHexDigest(seedHashHex) {
		return nil, fmt.Errorf("output: invalid seed hash %q", seedHashHex)
	}
	seed, err := a.loadSeed(seedHashHex)
	if err != nil {
		return nil, err
	}
	expected, err := masterseed.DigestFromHex(seedHashHex)
	if err != nil {
		return nil, fmt.Errorf("output: invalid seed hash: %w", err)
	}
	info, err := masterseed.VerifySeed(ctx, bytes.NewReader(seed), expected)
	if err != nil {
		return nil, fmt.Errorf("output: staged seed does not verify: %w", err)
	}
	payloads, sourceSize, err := a.loadBlocks(ctx, seed, info, expected)
	if err != nil {
		return nil, err
	}
	total := int64(sourceSize)

	tempName := seedHashHex + TempFileSuffix
	tempPath := filepath.Join(a.dir, tempName)

	// An already finalized download is returned as-is (idempotent retry).
	if final, err := a.Final(); err != nil {
		return nil, err
	} else if final != nil && final.SeedHashHex == seedHashHex {
		if err := a.verifyFile(final.Path, seed, info, expected, sourceSize); err == nil {
			// A stale pending record from a crashed finalization is cleaned up
			// now that the file is provably in place.
			if pending, err := a.loadPending(); err != nil {
				return nil, err
			} else if pending != nil && pending.SeedHashHex == seedHashHex {
				if err := a.clearPending(); err != nil {
					return nil, err
				}
			}
			return &Result{Path: final.Path, Bytes: final.Bytes, Reused: true}, nil
		}
	}

	// Resume an interrupted finalization before doing any work. The target is
	// only adopted after its content verifies against the seed.
	pending, err := a.loadPending()
	if err != nil {
		return nil, err
	}
	if pending != nil && pending.SeedHashHex == seedHashHex {
		target := filepath.Join(a.dir, pending.FinalName)
		if err := a.verifyFile(target, seed, info, expected, sourceSize); err == nil {
			// The link/rename already happened; the temp file may be left over
			// depending on where the crash occurred.
			if _, statErr := os.Stat(tempPath); statErr == nil {
				_ = os.Remove(tempPath)
			}
			final, err := json.MarshalIndent(FinalFile{SeedHashHex: seedHashHex, Path: target, Bytes: total}, "", "  ")
			if err != nil {
				return nil, err
			}
			if err := a.store.Save(finalPath, append(final, '\n')); err != nil {
				return nil, err
			}
			if err := a.clearPending(); err != nil {
				return nil, err
			}
			return &Result{Path: target, Bytes: total, Reused: true}, nil
		}
		// The pending target is not our content: fall through and pick a
		// fresh name instead of touching it.
	}

	if err := writeAssembled(tempPath, payloads); err != nil {
		return nil, err
	}
	name := ""
	if pending != nil && pending.SeedHashHex == seedHashHex {
		name = pending.FinalName
	}
	if name == "" {
		name = SanitizeFilename(preferredName, seedHashHex)
	}
	var target string
	published := false
	for attempt := 0; attempt < 10; attempt++ {
		candidate := name
		if attempt > 0 {
			extension := filepath.Ext(name)
			base := strings.TrimSuffix(name, extension)
			candidate = fmt.Sprintf("%s (%d)%s", base, attempt, extension)
		}
		unique, err := a.uniqueName(candidate)
		if err != nil {
			return nil, err
		}
		encoded, err := json.MarshalIndent(PendingRename{SeedHashHex: seedHashHex, FinalName: unique, TotalBytes: total}, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("output: encode pending rename: %w", err)
		}
		if err := a.store.Save(pendingPath, append(encoded, '\n')); err != nil {
			return nil, err
		}
		target = filepath.Join(a.dir, unique)
		err = os.Link(tempPath, target)
		if err == nil {
			published = true
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("output: finalize %s: %w", unique, err)
		}
	}
	if !published {
		return nil, errors.New("output: could not find a free final name")
	}
	_ = os.Remove(tempPath)
	final, err := json.MarshalIndent(FinalFile{SeedHashHex: seedHashHex, Path: target, Bytes: total}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("output: encode final record: %w", err)
	}
	if err := a.store.Save(finalPath, append(final, '\n')); err != nil {
		return nil, err
	}
	if err := a.clearPending(); err != nil {
		return nil, err
	}
	return &Result{Path: target, Bytes: total}, nil
}

func (a *Assembler) loadSeed(seedHashHex string) ([]byte, error) {
	for _, name := range []string{SeedStagingPath, "staging/" + seedHashHex} {
		raw, err := a.store.Load(name)
		if err == nil && len(raw) > 0 {
			return raw, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("output: seed %s is not staged", seedHashHex)
}

// loadBlocks loads and verifies every block in seed order, returning the
// payloads and the derived source size. The last staged block determines the
// source size, exactly like the protocol's block-count rules.
func (a *Assembler) loadBlocks(ctx context.Context, seed []byte, info masterseed.SeedInfo, expected masterseed.Digest) ([][]byte, uint64, error) {
	blockCount := info.BlockCount
	if blockCount == 0 {
		return nil, 0, nil
	}
	hashes := make([]masterseed.Digest, 0, blockCount)
	payloads := make([][]byte, 0, blockCount)
	for index := uint64(0); index < blockCount; index++ {
		hash, err := masterseed.ReadBlockHash(ctx, bytes.NewReader(seed), info.SeedSize, index)
		if err != nil {
			return nil, 0, fmt.Errorf("output: read seed block %d: %w", index, err)
		}
		hashes = append(hashes, hash)
		raw, err := a.store.Load("staging/" + hash.Hex())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, 0, fmt.Errorf("%w: block %d is missing", ErrIncomplete, index)
			}
			return nil, 0, err
		}
		payloads = append(payloads, raw)
	}
	sourceSize := (blockCount-1)*masterseed.BlockSize + uint64(len(payloads[blockCount-1]))
	for index := uint64(0); index < blockCount; index++ {
		want, err := masterseed.ExpectedBlockSize(sourceSize, index)
		if err != nil {
			return nil, 0, fmt.Errorf("output: expected size for block %d: %w", index, err)
		}
		if uint64(len(payloads[index])) != want {
			return nil, 0, fmt.Errorf("output: block %d has %d bytes, want %d", index, len(payloads[index]), want)
		}
		digest := masterseed.Sum256(payloads[index])
		if !digest.Equal(hashes[index]) {
			return nil, 0, fmt.Errorf("output: block %d does not hash to its seed entry", index)
		}
		if _, err := masterseed.VerifyBlockInSeed(ctx, bytes.NewReader(seed), expected, sourceSize, payloads[index]); err != nil {
			return nil, 0, fmt.Errorf("output: block %d is not in the seed: %w", index, err)
		}
	}
	return payloads, sourceSize, nil
}

// verifyFile re-verifies an already renamed file against the seed.
func (a *Assembler) verifyFile(path string, seed []byte, info masterseed.SeedInfo, expected masterseed.Digest, sourceSize uint64) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	for index := uint64(0); index < info.BlockCount; index++ {
		want, err := masterseed.ExpectedBlockSize(sourceSize, index)
		if err != nil {
			return err
		}
		buffer := make([]byte, want)
		if _, err := io.ReadFull(file, buffer); err != nil {
			return err
		}
		if _, err := masterseed.VerifyBlockInSeed(context.Background(), bytes.NewReader(seed), expected, sourceSize, buffer); err != nil {
			return fmt.Errorf("output: renamed file block %d does not match the seed: %w", index, err)
		}
	}
	if _, err := file.Read(make([]byte, 1)); err != io.EOF {
		return errors.New("output: renamed file has trailing bytes")
	}
	return nil
}

func (a *Assembler) loadPending() (*PendingRename, error) {
	raw, err := a.store.Load(pendingPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pending PendingRename
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, fmt.Errorf("output: pending rename is corrupt: %w", err)
	}
	if pending.FinalName == "" || !isHexDigest(pending.SeedHashHex) {
		return nil, errors.New("output: pending rename is invalid")
	}
	return &pending, nil
}

func (a *Assembler) clearPending() error {
	if err := os.Remove(filepath.Join(a.store.Root(), filepath.FromSlash(pendingPath))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("output: clear pending rename: %w", err)
	}
	return nil
}

func writeAssembled(path string, payloads [][]byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".assemble-*.tmp")
	if err != nil {
		return fmt.Errorf("output: create temporary file: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	for _, payload := range payloads {
		if _, err := temporary.Write(payload); err != nil {
			temporary.Close()
			return fmt.Errorf("output: write temporary file: %w", err)
		}
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("output: publish temporary file: %w", err)
	}
	return nil
}

// uniqueName returns name, or name.1/name.2/... before the extension when the
// name is already taken. Existing files are never overwritten.
func (a *Assembler) uniqueName(name string) (string, error) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := name
	for attempt := 1; attempt <= 10_000; attempt++ {
		_, err := os.Lstat(filepath.Join(a.dir, candidate))
		if errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
		if err != nil {
			return "", fmt.Errorf("output: inspect %s: %w", candidate, err)
		}
		candidate = fmt.Sprintf("%s.%d%s", base, attempt, ext)
	}
	return "", fmt.Errorf("output: too many files named like %s", name)
}

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeFilename turns a seller-recommended display name into a safe single
// path segment: the last path component, control characters removed, capped at
// 255 UTF-8 bytes, Windows device names and trailing dots/spaces neutralized.
// It never returns an empty name; the fallback is used instead.
func SanitizeFilename(recommended, fallback string) string {
	basename := recommended
	if index := strings.LastIndexAny(basename, `/\`); index >= 0 {
		basename = basename[index+1:]
	}
	var builder strings.Builder
	for _, character := range basename {
		if character < 0x20 || character == 0x7f || (character >= 0x80 && character <= 0x9f) {
			continue
		}
		builder.WriteRune(character)
	}
	name := builder.String()
	name = strings.TrimRight(name, " .")
	if name == "" || name == "." || name == ".." {
		return fallback
	}
	if len(name) > maxNameBytes {
		name = truncateUTF8(name, maxNameBytes)
	}
	if name == "" {
		return fallback
	}
	device := name
	if index := strings.IndexByte(device, '.'); index >= 0 {
		device = device[:index]
	}
	if windowsReserved[strings.ToUpper(device)] {
		name = "_" + name
	}
	return name
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := value[:limit]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

// RenameFinal publishes the recorded final file under a user-chosen name.
// The link is atomic and never overwrites an existing target; a rename intent
// with the source content hash makes a crash between the link and the
// final-record update recoverable, and a replaced target is rejected instead
// of deleting the verified original.
func (a *Assembler) RenameFinal(chosen string) (*FinalFile, error) {
	final, err := a.Final()
	if err != nil {
		return nil, err
	}
	if final == nil {
		return nil, errors.New("output: no final file to rename")
	}
	oldPath := final.Path
	name := SanitizeFilename(chosen, filepath.Base(oldPath))
	if name == filepath.Base(oldPath) {
		return final, nil
	}
	digest, err := fileSHA256(oldPath)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 10; attempt++ {
		candidate := name
		if attempt > 0 {
			extension := filepath.Ext(name)
			base := strings.TrimSuffix(name, extension)
			candidate = fmt.Sprintf("%s (%d)%s", base, attempt, extension)
		}
		unique, err := a.uniqueName(candidate)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(a.dir, unique)
		// Link is atomic and fails with EEXIST instead of overwriting a file
		// created after uniqueName's check.
		if err := os.Link(oldPath, target); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return nil, fmt.Errorf("output: publish final file: %w", err)
		}
		intent := renameIntent{SeedHashHex: final.SeedHashHex, From: oldPath, To: target, Bytes: final.Bytes, SHA256: digest}
		encoded, err := json.Marshal(intent)
		if err != nil {
			_ = os.Remove(target)
			return nil, err
		}
		if err := a.store.Save(renameIntentPath, append(encoded, '\n')); err != nil {
			_ = os.Remove(target)
			return nil, err
		}
		final.Path = target
		_ = os.Remove(oldPath)
		encodedFinal, err := json.Marshal(final)
		if err != nil {
			return nil, err
		}
		if err := a.store.Save(finalPath, append(encodedFinal, '\n')); err != nil {
			return nil, err
		}
		_ = a.store.Remove(renameIntentPath)
		return final, nil
	}
	return nil, errors.New("output: could not find a free final name")
}

// recoverRename completes a publish that crashed before the final record was
// updated. The intent is written only after the atomic link succeeded.
func (a *Assembler) recoverRename() error {
	raw, err := a.store.Load(renameIntentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var intent renameIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return fmt.Errorf("output: rename intent is corrupt: %w", err)
	}
	fromInfo, fromErr := os.Stat(intent.From)
	toInfo, toErr := os.Stat(intent.To)
	fromExists := fromErr == nil
	toExists := toErr == nil
	switch {
	case toExists && fromExists:
		// Only a hard link to the verified original may be removed. A target
		// replaced by an unrelated file keeps the original and reports the
		// conflict instead of pointing final.json at foreign content.
		if !os.SameFile(fromInfo, toInfo) {
			return fmt.Errorf("output: rename target %s is not the verified original; keep %s and inspect", intent.To, intent.From)
		}
		if err := os.Remove(intent.From); err != nil {
			return fmt.Errorf("output: recover rename: %w", err)
		}
	case toExists:
		// The old link is gone; verify the target content before adopting it.
		if intent.SHA256 == "" || toInfo.Size() != intent.Bytes {
			return fmt.Errorf("output: rename target %s has no verifiable identity", intent.To)
		}
		digest, err := fileSHA256(intent.To)
		if err != nil {
			return err
		}
		if digest != intent.SHA256 {
			return fmt.Errorf("output: rename target %s was replaced; keep original and inspect", intent.To)
		}
	case fromExists:
		// The target disappeared after the link; keep the original final.
		return a.store.Remove(renameIntentPath)
	default:
		return errors.New("output: rename intent points to no file")
	}
	encoded, err := json.Marshal(FinalFile{SeedHashHex: intent.SeedHashHex, Path: intent.To, Bytes: intent.Bytes})
	if err != nil {
		return err
	}
	if err := a.store.Save(finalPath, append(encoded, '\n')); err != nil {
		return err
	}
	return a.store.Remove(renameIntentPath)
}

// fileSHA256 hashes a published final file for identity checks.
func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
