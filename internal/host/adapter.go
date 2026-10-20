// Package host wires the download scheduler kernel to real execution.
//
// It owns three pieces:
//
//   - Adapter: the persisted kernel state plus the command outbox
//     (scheduler/tasks/<taskId>.json). Facts enter through Handle, commands
//     leave through the outbox and are only removed by Settle.
//   - Executor: commandId-idempotent dispatch to the execution ports.
//   - BuyerPorts: the buyer execution layer (go-bitfs steps over the client
//     store/fund ledger) behind an injected peer for protocol transport.
package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
	"github.com/bsv8/keymaster.cc-cli/internal/store"
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
	Format   string          `json:"format"`
	Version  int             `json:"version"`
	TaskID   string          `json:"taskId"`
	SeedHash string          `json:"seedHash"`
	State    scheduler.State `json:"state"`
	Outbox   []OutboxEntry   `json:"outbox"`
}

// AdapterInit describes a fresh task; it is ignored when a snapshot exists.
type AdapterInit struct {
	TaskID        string
	SeedHash      string
	Config        scheduler.Config
	Content       []scheduler.Content
	ManifestKnown bool
	Reused        []string
	// Clock supplies ReceivedAt/OccurredAt defaults for events that do not
	// carry them. It must be monotonic.
	Clock func() int64
}

// Adapter is one task's scheduler host state.
type Adapter struct {
	store    *store.Store
	path     string
	taskID   string
	seedHash string
	clock    func() int64
	state    scheduler.State
	outbox   []OutboxEntry
}

// OpenAdapter restores the snapshot for init.TaskID or creates the task and
// persists it immediately.
func OpenAdapter(s *store.Store, init AdapterInit) (*Adapter, error) {
	if s == nil {
		return nil, errors.New("host: store is required")
	}
	if init.TaskID == "" || init.SeedHash == "" {
		return nil, errors.New("host: taskId and seedHash are required")
	}
	adapter := &Adapter{
		store:    s,
		path:     "scheduler/tasks/" + init.TaskID + ".json",
		taskID:   init.TaskID,
		seedHash: init.SeedHash,
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
		if stored.TaskID != init.TaskID || stored.SeedHash != init.SeedHash {
			return nil, errors.New("host: scheduler snapshot belongs to another task")
		}
		if stored.State.TaskID != init.TaskID || stored.State.SeedHash != init.SeedHash {
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
	state, err := scheduler.CreateTask(scheduler.CreateInput{
		TaskID:        init.TaskID,
		SeedHash:      init.SeedHash,
		Config:        init.Config,
		Content:       init.Content,
		ManifestKnown: init.ManifestKnown,
		Reused:        init.Reused,
	})
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
		TaskID:   a.taskID,
		SeedHash: a.seedHash,
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
func (a *Adapter) Handle(event scheduler.Event) (*scheduler.Result, error) {
	if event.EventID == "" {
		return nil, errors.New("host: eventId is required")
	}
	if event.TaskID == "" {
		event.TaskID = a.taskID
	}
	if event.TaskID != a.taskID {
		return nil, errors.New("host: event belongs to another task")
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
