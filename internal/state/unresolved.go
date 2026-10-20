// Package state stores client-level records that are independent of any
// single download task, such as the unresolved funding history.
package state

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Unresolved is one transaction or session whose final result is not known.
// Records live under ~/.km/client/unresolved/ and are never deleted by
// per-seedHash cleanup.
type Unresolved struct {
	RecordedAt time.Time `json:"recordedAt"`
	Kind       string    `json:"kind"`
	TxID       string    `json:"txid,omitempty"`
	RawTxHex   string    `json:"rawTxHex,omitempty"`
	Reason     string    `json:"reason"`
}

const fileName = "unresolved.jsonl"

// Append writes one unresolved record. The directory is created with 0700 and
// the file with 0600; writes are append-only.
func Append(directory string, record Unresolved) error {
	if directory == "" {
		return fmt.Errorf("state: unresolved directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("state: create unresolved directory: %w", err)
	}
	path := filepath.Join(directory, fileName)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("state: open unresolved history: %w", err)
	}
	defer file.Close()
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("state: encode unresolved record: %w", err)
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("state: write unresolved record: %w", err)
	}
	return nil
}

// List reads the unresolved history. A missing file returns an empty slice.
func List(directory string) ([]Unresolved, error) {
	path := filepath.Join(directory, fileName)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("state: open unresolved history: %w", err)
	}
	defer file.Close()
	records := make([]Unresolved, 0)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record Unresolved
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("state: invalid unresolved record: %w", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("state: read unresolved history: %w", err)
	}
	return records, nil
}
