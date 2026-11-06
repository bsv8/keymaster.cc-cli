// Package host wires the download scheduler kernel to real execution.
//
// It owns three pieces:
//
//   - Adapter: the persisted kernel state plus the command outbox
//     (scheduler/tasks/<seed>.<exec>.json, one file per execution; the
//     current pointer lives in scheduler/tasks/<seed>.current). Facts enter
//     through Handle, commands leave through the outbox and are only removed
//     by Settle.
//   - Executor: commandId-idempotent dispatch to the execution ports.
//   - BuyerPorts: the buyer execution layer (BitFSProtocol steps over the client
//     store/fund ledger) behind an injected peer for protocol transport.
package host

import (
	"encoding/json"
	"errors"
	"fmt"
	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
	"os"

	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
)

// SnapshotFormat identifies the persisted scheduler host snapshot.
const SnapshotFormat = "keymaster.scheduler-host"

// SnapshotVersion is the snapshot schema version.
const SnapshotVersion = 1

// OutboxEntry is one command that has been emitted but not yet confirmed by an
// execution fact.
type OutboxEntry struct {
	CommandID string            `json:"commandId"`
	Type      string            `json:"type"`
	Revision  int64             `json:"revision"`
	Command   scheduler.Command `json:"command"`
}

type snapshot struct {
	Format   string `json:"format"`
	Version  int    `json:"version"`
	SeedHash string `json:"seedHash"`
	// ExecID scopes one purchase execution of the seed ("e1", "e2", ...).
	// Every execution uses the same prefixed host keys
	// ("e<N>/<seedHash>/pool/..."); there is no bare-ID branch.
	ExecID string          `json:"execId"`
	State  scheduler.State `json:"state"`
	Outbox []OutboxEntry   `json:"outbox"`
}

// AdapterInit describes one execution's task; it is ignored when that
// execution's snapshot already exists. The Scheduler instance carries the
// shared kernel configuration.
type AdapterInit struct {
	// Scheduler is the shared download scheduler instance (owner/network lifecycle).
	Scheduler *scheduler.Scheduler
	// Create carries the task identity and (optionally) the verified local file info.
	Create scheduler.CreateTaskInput
	// ExecID scopes one purchase execution of the seed ("e1", "e2", ...).
	// Every execution uses the same prefixed host keys. The per-execution
	// snapshot lives in scheduler/tasks/<seed>.<exec>.json; the current
	// pointer lives in scheduler/tasks/<seed>.current.
	ExecID string
	// Clock supplies ReceivedAt/OccurredAt defaults for events that do not
	// carry them. It must be monotonic.
	Clock func() int64
}

// Adapter is one execution's scheduler host state. The snapshot file holds
// that execution's kernel state and outbox; the seed-level pointer decides
// which execution is current.
type Adapter struct {
	store    *store.Store
	path     string
	seedHash string
	execID   string
	clock    func() int64
	state    scheduler.State
	outbox   []OutboxEntry
}

// ExecID returns the execution this adapter drives.
func (a *Adapter) ExecID() string { return a.execID }

// SeedHash returns the seed this adapter drives.
func (a *Adapter) SeedHash() string { return a.seedHash }

// OpenAdapter restores the snapshot for init.Create.SeedHash + init.ExecID or
// creates that execution's task and persists it immediately. Callers resolve
// the current execution via CurrentExecID and create new executions via
// CreateExecution + SwitchCurrent; this function never touches the pointer.
func OpenAdapter(s *store.Store, init AdapterInit) (*Adapter, error) {
	if s == nil {
		return nil, errors.New("host: store is required")
	}
	if init.Create.SeedHash == "" {
		return nil, errors.New("host: seedHash is required")
	}
	if !ValidateExecID(init.ExecID) {
		return nil, errors.New("host: valid execID (e1, e2, ...) is required")
	}
	adapter := &Adapter{
		store:    s,
		path:     ExecSnapshotPath(init.Create.SeedHash, init.ExecID),
		seedHash: init.Create.SeedHash,
		execID:   init.ExecID,
		clock:    init.Clock,
	}
	if adapter.clock == nil {
		adapter.clock = func() int64 { return 0 }
	}
	raw, err := s.Load(adapter.path)
	if err == nil {
		var stored snapshot
		if err := json.Unmarshal(raw, &stored); err != nil {
			return nil, fmt.Errorf("host: scheduler snapshot is corrupt: %w", err)
		}
		if stored.Format != SnapshotFormat || stored.Version != SnapshotVersion {
			return nil, errors.New("host: unsupported scheduler snapshot")
		}
		if stored.SeedHash != init.Create.SeedHash {
			return nil, errors.New("host: scheduler snapshot belongs to another download")
		}
		if stored.ExecID != init.ExecID {
			return nil, errors.New("host: scheduler snapshot belongs to another execution")
		}
		if stored.State.SeedHash != init.Create.SeedHash {
			return nil, errors.New("host: scheduler snapshot state does not match its header")
		}
		state, err := scheduler.RestoreTask(stored.State)
		if err != nil {
			return nil, fmt.Errorf("host: scheduler snapshot state is invalid: %w", err)
		}
		outbox, err := validateOutbox(stored.Outbox)
		if err != nil {
			return nil, err
		}
		adapter.state = state
		adapter.outbox = outbox
		return adapter, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if init.Scheduler == nil {
		return nil, errors.New("host: scheduler instance is required")
	}
	state, err := init.Scheduler.CreateTask(init.Create)
	if err != nil {
		return nil, err
	}
	adapter.state = state
	if err := adapter.save(); err != nil {
		return nil, err
	}
	return adapter, nil
}

func validateOutbox(entries []OutboxEntry) ([]OutboxEntry, error) {
	outbox := make([]OutboxEntry, 0, len(entries))
	for index, entry := range entries {
		if entry.CommandID == "" || entry.Type == "" {
			return nil, fmt.Errorf("host: outbox entry %d is incomplete", index)
		}
		if entry.Command.CommandID != entry.CommandID || entry.Command.Type != entry.Type {
			return nil, fmt.Errorf("host: outbox entry %d does not match its command", index)
		}
		outbox = append(outbox, entry)
	}
	return outbox, nil
}

func (a *Adapter) save() error {
	encoded, err := json.MarshalIndent(snapshot{
		Format:   SnapshotFormat,
		Version:  SnapshotVersion,
		SeedHash: a.seedHash,
		ExecID:   a.execID,
		State:    a.state,
		Outbox:   a.outbox,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("host: encode scheduler snapshot: %w", err)
	}
	return a.store.Save(a.path, append(encoded, '\n'))
}

// Handle applies one fact, persists the new state with the extended outbox and
// returns the step result. The caller executes the returned commands.
func (a *Adapter) Handle(event scheduler.Event) (output *scheduler.Result, failure error) {
	bitfsdebug.Log("keymaster.cc-cli", "scheduler.event.received", func() any { return map[string]any{"seedHashHex": a.seedHash, "event": event} })
	defer func() {
		bitfsdebug.Log("keymaster.cc-cli", "scheduler.decision", func() any {
			return map[string]any{"seedHashHex": a.seedHash, "result": output, "state": a.state, "error": failure}
		})
	}()
	if event.EventID == "" {
		return nil, errors.New("host: eventId is required")
	}
	if event.SeedHash == "" {
		event.SeedHash = a.seedHash
	}
	if event.SeedHash != a.seedHash {
		return nil, errors.New("host: event belongs to another download")
	}
	if event.OccurredAt == 0 {
		event.OccurredAt = a.clock()
	}
	if event.ReceivedAt == 0 {
		event.ReceivedAt = a.clock()
	}
	result, err := scheduler.Step(a.state, event)
	if err != nil {
		return nil, err
	}
	a.state = result.State
	for _, command := range result.Commands {
		if a.hasCommand(command.CommandID) {
			continue
		}
		a.outbox = append(a.outbox, OutboxEntry{
			CommandID: command.CommandID,
			Type:      command.Type,
			Revision:  command.Revision,
			Command:   command,
		})
	}
	if err := a.save(); err != nil {
		return nil, err
	}
	return &result, nil
}

// Settle removes a confirmed command from the outbox.
func (a *Adapter) Settle(commandID string) error {
	next := make([]OutboxEntry, 0, len(a.outbox))
	for _, entry := range a.outbox {
		if entry.CommandID != commandID {
			next = append(next, entry)
		}
	}
	if len(next) == len(a.outbox) {
		return nil
	}
	a.outbox = next
	return a.save()
}

func (a *Adapter) hasCommand(commandID string) bool {
	for _, entry := range a.outbox {
		if entry.CommandID == commandID {
			return true
		}
	}
	return false
}

// State returns a copy of the kernel state.
func (a *Adapter) State() scheduler.State { return a.state }

// View returns the host projection.
func (a *Adapter) View() scheduler.View { return scheduler.ProjectView(a.state) }

// NextWakeAt returns the timer instant the kernel asks for, if any.
func (a *Adapter) NextWakeAt() *int64 { return scheduler.NextWakeAt(a.state) }

// Pending returns the unconfirmed commands.
func (a *Adapter) Pending() []OutboxEntry { return append([]OutboxEntry(nil), a.outbox...) }
