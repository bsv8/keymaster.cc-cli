package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"

	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

// Execution identity for one seed download.
//
// The kernel restarts its local numbering (pool/request/command sequence) at
// zero for every task, so kernel pool IDs repeat across executions of the
// same seed ("<seedHash>/pool/1", ...). Host-side identities are therefore
// namespaced with the purchase execution id: ledger pools, pool journals,
// pool sessions and in-memory pool routing all use ExecPoolID, while events
// fed back into the kernel always carry the raw kernel pool ID.
//
// All executions use the same format: the first execution is "e1" and its
// host keys are "e1/<seedHash>/pool/1", the second "e2/<seedHash>/pool/1",
// and so on. There is no bare-ID compatibility branch: the system is
// unreleased and old bare keys (if any) are left in place but never matched.
//
// Each execution owns an independent snapshot file holding both kernel state
// and command outbox:
//
//	scheduler/tasks/<seed>.<exec>.json
//
// The current execution pointer is a separate atomic file:
//
//	scheduler/tasks/<seed>.current   (plain text "e2\n")
//
// Creation writes the new snapshot file first, then switches the pointer.
// If the process crashes between the two, the pointer still names the old
// execution (safe) and the orphan snapshot is reused by the next creation
// when it is still fresh (revision 0), otherwise left as history.

// ValidateExecID reports whether execID has the "e<N>" form.
func ValidateExecID(execID string) bool {
	if len(execID) < 2 || execID[0] != 'e' {
		return false
	}
	n, err := strconv.Atoi(execID[1:])
	return err == nil && n >= 1
}

// ExecPoolID namespaces a kernel pool ID with its purchase execution.
// All executions use the same prefixed form; there is no bare fallback.
func ExecPoolID(execID, poolID string) string {
	return execID + "/" + poolID
}

// ExecPoolPrefix returns the ledger/journal key prefix selecting exactly one
// execution's pools of a seed.
func ExecPoolPrefix(execID, seedHash string) string {
	return execID + "/" + seedHash + "/pool/"
}

// xpool maps a kernel pool ID from a command to this execution's host-side
// pool key. Kernel events keep the raw ID; storage, ledger, journals and
// session routing use the mapped one.
func (p *BuyerPorts) xpool(poolID string) string {
	return ExecPoolID(p.ExecID, poolID)
}

// SplitExecPoolID reverses ExecPoolID for diagnostics. ok is false when the
// key does not belong to the given execution.
func SplitExecPoolID(execID, key string) (poolID string, ok bool) {
	prefix := execID + "/"
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	return strings.TrimPrefix(key, prefix), true
}

// ExecSnapshotPath returns the per-execution snapshot file for a seed.
func ExecSnapshotPath(seedHash, execID string) string {
	return "scheduler/tasks/" + seedHash + "." + execID + ".json"
}

// PointerPath returns the current-execution pointer file for a seed.
func PointerPath(seedHash string) string {
	return "scheduler/tasks/" + seedHash + ".current"
}

// parseExecFileName extracts the exec id from a "scheduler/tasks/<seed>.<exec>.json"
// file name. ok is false for pointer files and unrelated entries.
func parseExecFileName(seedHash, name string) (string, bool) {
	base := name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	prefix := seedHash + "."
	suffix := ".json"
	if !strings.HasPrefix(base, prefix) || !strings.HasSuffix(base, suffix) {
		return "", false
	}
	execID := strings.TrimSuffix(strings.TrimPrefix(base, prefix), suffix)
	if !ValidateExecID(execID) {
		return "", false
	}
	return execID, true
}

// CurrentExecID resolves which execution the pointer names. A missing pointer
// means no execution was ever created for this seed.
func CurrentExecID(s *store.Store, seedHash string) (execID string, exists bool, err error) {
	raw, err := s.Load(PointerPath(seedHash))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("host: read execution pointer: %w", err)
	}
	execID = strings.TrimSpace(string(raw))
	if execID == "" {
		return "", false, errors.New("host: execution pointer is empty")
	}
	if !ValidateExecID(execID) {
		return "", false, fmt.Errorf("host: execution pointer %q is invalid", execID)
	}
	return execID, true, nil
}

// ListExecs returns all execution ids that own a snapshot file, oldest first.
func ListExecs(s *store.Store, seedHash string) ([]string, error) {
	files, err := s.List("scheduler/tasks/")
	if err != nil {
		return nil, fmt.Errorf("host: list scheduler snapshots: %w", err)
	}
	var out []string
	for _, name := range files {
		if id, ok := parseExecFileName(seedHash, name); ok {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return execNumber(out[i]) < execNumber(out[j])
	})
	return out, nil
}

func execNumber(execID string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(execID, "e"))
	return n
}

// NextExecID derives the id for a fresh execution: one more than the highest
// existing execution, or "e1" when none exists.
func NextExecID(s *store.Store, seedHash string) (string, error) {
	execs, err := ListExecs(s, seedHash)
	if err != nil {
		return "", err
	}
	max := 0
	for _, id := range execs {
		if n := execNumber(id); n > max {
			max = n
		}
	}
	return fmt.Sprintf("e%d", max+1), nil
}

// execRevision reads only the kernel revision of an execution snapshot.
// It is used to detect fresh orphans that can be safely reused.
func execRevision(s *store.Store, seedHash, execID string) (int64, bool, error) {
	raw, err := s.Load(ExecSnapshotPath(seedHash, execID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("host: read execution snapshot: %w", err)
	}
	var stored snapshot
	if err := json.Unmarshal(raw, &stored); err != nil {
		return 0, false, fmt.Errorf("host: execution snapshot is corrupt: %w", err)
	}
	return stored.State.Revision, true, nil
}

// CreateExecution writes a fresh execution snapshot file and returns its id.
// It does not switch the current pointer: the caller switches after the file
// is durable, so a crash between creation and switch leaves the old pointer
// valid. A fresh orphan (revision 0, never driven) is reused instead of
// allocating a new id, so interrupted creations do not accumulate.
func CreateExecution(s *store.Store, sched *scheduler.Scheduler, seedHash string, create scheduler.CreateTaskInput) (string, error) {
	if s == nil || sched == nil {
		return "", errors.New("host: store and scheduler are required")
	}
	if seedHash == "" {
		return "", errors.New("host: seedHash is required")
	}
	if create.SeedHash != "" && create.SeedHash != seedHash {
		return "", errors.New("host: create input belongs to another download")
	}
	create.SeedHash = seedHash
	current, _, err := CurrentExecID(s, seedHash)
	if err != nil {
		return "", err
	}
	execs, err := ListExecs(s, seedHash)
	if err != nil {
		return "", err
	}
	// Reuse a fresh orphan first: it was created but never became current.
	for _, id := range execs {
		if id == current {
			continue
		}
		revision, exists, err := execRevision(s, seedHash, id)
		if err != nil {
			return "", err
		}
		if exists && revision == 0 {
			state, err := sched.CreateTask(create)
			if err != nil {
				return "", err
			}
			if err := saveExecSnapshot(s, seedHash, id, state, nil); err != nil {
				return "", err
			}
			return id, nil
		}
	}
	next, err := NextExecID(s, seedHash)
	if err != nil {
		return "", err
	}
	state, err := sched.CreateTask(create)
	if err != nil {
		return "", err
	}
	if err := saveExecSnapshot(s, seedHash, next, state, nil); err != nil {
		return "", err
	}
	return next, nil
}

// saveExecSnapshot persists one execution's kernel state plus outbox.
func saveExecSnapshot(s *store.Store, seedHash, execID string, state scheduler.State, outbox []OutboxEntry) error {
	encoded, err := json.MarshalIndent(snapshot{
		Format:   SnapshotFormat,
		Version:  SnapshotVersion,
		SeedHash: seedHash,
		ExecID:   execID,
		State:    state,
		Outbox:   outbox,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("host: encode scheduler snapshot: %w", err)
	}
	if err := s.Save(ExecSnapshotPath(seedHash, execID), append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

// SwitchCurrent atomically points the seed at an existing execution. The
// target snapshot must already exist; the pointer write itself is atomic, so
// a crash either keeps the old pointer or installs the new one, never a
// half-written value.
func SwitchCurrent(s *store.Store, seedHash, execID string) error {
	if !ValidateExecID(execID) {
		return fmt.Errorf("host: invalid exec id %q", execID)
	}
	if exists, _ := s.Exists(ExecSnapshotPath(seedHash, execID)); !exists {
		return fmt.Errorf("host: execution %s has no snapshot", execID)
	}
	if err := s.Save(PointerPath(seedHash), []byte(execID+"\n")); err != nil {
		return fmt.Errorf("host: switch execution pointer: %w", err)
	}
	return nil
}

// LoadExecSnapshot loads one execution's snapshot for inspection. Archived
// executions are read-only history; only the current execution is resumed by
// the download loop, but any execution can be inspected and can be made
// current again with SwitchCurrent to finish its delivery, close or refund.
func LoadExecSnapshot(s *store.Store, seedHash, execID string) (scheduler.State, []OutboxEntry, error) {
	raw, err := s.Load(ExecSnapshotPath(seedHash, execID))
	if err != nil {
		return scheduler.State{}, nil, fmt.Errorf("host: read execution snapshot: %w", err)
	}
	var stored snapshot
	if err := json.Unmarshal(raw, &stored); err != nil {
		return scheduler.State{}, nil, fmt.Errorf("host: execution snapshot is corrupt: %w", err)
	}
	if stored.SeedHash != "" && stored.SeedHash != seedHash {
		return scheduler.State{}, nil, errors.New("host: execution snapshot belongs to another download")
	}
	if stored.ExecID != "" && stored.ExecID != execID {
		return scheduler.State{}, nil, errors.New("host: execution snapshot belongs to another execution")
	}
	state, err := scheduler.RestoreTask(stored.State)
	if err != nil {
		return scheduler.State{}, nil, fmt.Errorf("host: execution snapshot state is invalid: %w", err)
	}
	outbox, err := validateOutbox(stored.Outbox)
	if err != nil {
		return scheduler.State{}, nil, err
	}
	return state, outbox, nil
}

// RedownloadEligibility decides whether a stopped download may start a new
// execution. The gate is the terminal stopped state plus outstanding
// responsibilities, never the presence of a failure record: a normally
// cancelled/stopped execution with settled funds and no pending work is
// eligible, while any outstanding purchase authorization
// (pending/unresolved request), live / closing / problem pool funding, or a
// problem fund state means the old execution still holds responsibilities
// that a new execution must not silently abandon. A broadcast close
// ("closing") is still unconfirmed on chain and therefore blocks redownload
// like any other pending pool. Finished (completed) downloads are never
// eligible: success does not offer forced re-buy.
func RedownloadEligibility(state scheduler.State, view scheduler.View) (bool, string) {
	if view.Execution == "finished" {
		return false, "download is already finished; redownload is not offered for completed downloads"
	}
	if view.Execution != "stopped" {
		return false, "download has not reached a terminal stopped state"
	}
	for _, r := range state.Requests {
		if r.State == "pending" || r.State == "unresolved" {
			return false, "an outstanding purchase authorization is still unresolved"
		}
	}
	for _, p := range state.Pools {
		switch p.Funding {
		case "opening", "open":
			return false, "a pool still holds live funds"
		case "problem":
			return false, "a pool needs manual fund tracing"
		case "closing":
			return false, "a pool close is still unconfirmed"
		}
	}
	if view.Funds == "problem" {
		return false, "funds need manual tracing"
	}
	return true, ""
}
