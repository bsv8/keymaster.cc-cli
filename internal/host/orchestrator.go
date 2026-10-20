package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
)

// Orchestrator drives one task: facts enter through Feed, the kernel state and
// command outbox are persisted, and commands run through the executor. Ports
// enqueue follow-up facts with Enqueue instead of recursing, so a burst of
// command -> fact -> command stays bounded and reentrancy-safe.
type Orchestrator struct {
	Adapter  *Adapter
	Executor *Executor
	// MaxCommands bounds one Feed cycle; exceeding it means the kernel and the
	// host disagree and the cycle stops instead of looping forever.
	MaxCommands int

	queue []scheduler.Event
}

// NewOrchestrator wires an adapter and executor together.
func NewOrchestrator(adapter *Adapter, executor *Executor) (*Orchestrator, error) {
	if adapter == nil || executor == nil {
		return nil, errors.New("host: adapter and executor are required")
	}
	return &Orchestrator{Adapter: adapter, Executor: executor, MaxCommands: 128}, nil
}

// Enqueue records a follow-up fact produced by a command port.
func (o *Orchestrator) Enqueue(event scheduler.Event) error {
	if event.EventID == "" {
		return errors.New("host: eventId is required")
	}
	o.queue = append(o.queue, event)
	return nil
}

// Feed applies one fact and drains every command/fact cascade it triggers.
func (o *Orchestrator) Feed(ctx context.Context, event scheduler.Event) error {
	if err := o.Enqueue(event); err != nil {
		return err
	}
	return o.Drain(ctx)
}

// Drain executes queued facts until the queue is empty or the bound is hit.
func (o *Orchestrator) Drain(ctx context.Context) error {
	executed := 0
	for len(o.queue) > 0 {
		next := o.queue[0]
		o.queue = o.queue[1:]
		result, err := o.Adapter.Handle(next)
		if err != nil {
			return err
		}
		for _, command := range result.Commands {
			executed++
			if executed > o.MaxCommands {
				return fmt.Errorf("host: command cascade exceeded %d commands", o.MaxCommands)
			}
			if err := o.Executor.Run(ctx, command); err != nil {
				return err
			}
		}
	}
	return nil
}
