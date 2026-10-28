package wallet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	manager "github.com/bsv8/keymaster.cc-cli/utxo"
)

// Host storage envelope. The embedded manager state stays canonical utm/1 and
// cross-language compatible; result diagnostics commit with it in one rename.
type managerFile struct {
	Schema    string            `json:"schema"`
	StateBlob string            `json:"stateBlob"`
	Results   map[string]string `json:"results"`
}

const managerFileSchema = "km-manager-store/1"

func decodeManagerFile(raw []byte) (string, map[string]string, error) {
	var header struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return "", nil, fmt.Errorf("wallet: invalid manager file: %w", err)
	}
	if header.Schema == "" {
		return string(raw), map[string]string{}, nil
	} // legacy canonical manager file
	if header.Schema != managerFileSchema {
		return "", nil, fmt.Errorf("wallet: unsupported manager store schema %q", header.Schema)
	}
	var file managerFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return "", nil, err
	}
	if file.StateBlob == "" || file.Results == nil {
		return "", nil, fmt.Errorf("wallet: incomplete manager store")
	}
	return file.StateBlob, file.Results, nil
}

func (c *sharedCore) writeManagerBlobLocked(blob, commandID, resultJSON string) error {
	results := make(map[string]string, len(c.results)+1)
	for id, result := range c.results {
		results[id] = result
	}
	if commandID != "" {
		attempt, _ := json.Marshal(map[string]string{"commandId": commandID, "result": resultJSON})
		results[fmt.Sprintf("attempt#%d", len(results))] = string(attempt)
		results[commandID] = resultJSON
	}
	raw, err := json.Marshal(managerFile{Schema: managerFileSchema, StateBlob: blob, Results: results})
	if err == nil && c.statePath != "" {
		err = os.MkdirAll(filepath.Dir(c.statePath), 0700)
		if err == nil {
			err = atomicWriteFile(c.statePath, raw, 0600)
		}
	}
	if err != nil {
		c.degraded = true
		c.degradedErr = err
		return err
	}
	c.results = results
	return nil
}

// managerStore is the CLI's implementation of the same DurableStore port used
// by TS orchestration. It is only used under localMu/core.mu; no network here.
type managerStore struct {
	core       *sharedCore
	diagnostic *string
}

func (s managerStore) SaveIntent(_ string, _ string, blob string) error {
	return s.core.writeManagerBlobLocked(blob, "", "")
}
func (s managerStore) LoadIntent(id string) (string, bool) {
	raw, durable, err := s.core.kernel.SigningIntent(id)
	if err != nil || !durable {
		return "", false
	}
	encoded, err := json.Marshal(map[string]string{"allocationId": id, "unsignedTxHex": raw})
	return string(encoded), err == nil
}
func (s managerStore) SaveDispatch(_ string, _ string, _ string, blob string) error {
	return s.core.writeManagerBlobLocked(blob, "", "")
}
func (s managerStore) LoadExact(txid string) (string, bool) {
	for _, row := range s.core.kernel.OutboxView() {
		if row.TxID == txid {
			return row.RawTxHex, true
		}
	}
	return "", false
}
func (s managerStore) RecordResult(id, result, blob string) error {
	if s.diagnostic != nil && *s.diagnostic != "" {
		encoded, err := json.Marshal(map[string]string{"managerResult": result, "provider": *s.diagnostic})
		if err != nil {
			return err
		}
		result = string(encoded)
	}
	return s.core.writeManagerBlobLocked(blob, id, result)
}
func (s managerStore) SaveState(blob string) error {
	return s.core.writeManagerBlobLocked(blob, "", "")
}

var _ manager.DurableStore = managerStore{}
