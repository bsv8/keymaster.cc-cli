package host

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
)

// Ports executes one kernel command type. Implementations must persist any
// execution fact before reporting success; a returned error means the command
// stays in the outbox for retry or reconciliation.
type Ports interface {
	OpenPool(ctx context.Context, command scheduler.Command) error
	DispatchDownload(ctx context.Context, command scheduler.Command) error
	DrainPool(ctx context.Context, command scheduler.Command) error
	ClosePool(ctx context.Context, command scheduler.Command) error
}

// Executor dispatches commands idempotently by commandId. A command that
// fails is forgotten so a later retry can run again; the outbox is the
// cross-restart source of truth.
type Executor struct {
	ports Ports

	mu   sync.Mutex
	done map[string]bool
}

// NewExecutor creates an executor over ports.
func NewExecutor(ports Ports) (*Executor, error) {
	if ports == nil {
		return nil, errors.New("host: ports are required")
	}
	return &Executor{ports: ports, done: map[string]bool{}}, nil
}

// Run executes one command unless its commandId already ran successfully.
func (e *Executor) Run(ctx context.Context, command scheduler.Command) error {
	if command.CommandID == "" {
		return errors.New("host: commandId is required")
	}
	e.mu.Lock()
	if e.done[command.CommandID] {
		e.mu.Unlock()
		return nil
	}
	e.done[command.CommandID] = true
	e.mu.Unlock()

	var err error
	switch command.Type {
	case "open":
		err = e.ports.OpenPool(ctx, command)
	case "download":
		err = e.ports.DispatchDownload(ctx, command)
	case "drain":
		err = e.ports.DrainPool(ctx, command)
	case "close":
		err = e.ports.ClosePool(ctx, command)
	default:
		err = fmt.Errorf("host: unknown command type %q", command.Type)
	}
	if err != nil {
		e.mu.Lock()
		delete(e.done, command.CommandID)
		e.mu.Unlock()
		return err
	}
	return nil
}

// Forget drops the deduplication record for one command (or all when empty),
// for example after reconciliation rewrote the round's assumptions.
func (e *Executor) Forget(commandID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if commandID == "" {
		e.done = map[string]bool{}
		return
	}
	delete(e.done, commandID)
}
