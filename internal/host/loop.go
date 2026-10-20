package host

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/bsv8/keymaster.cc-cli/internal/scheduler"
)

// Loop is one task's single event loop. Every fact enters through Feed (or a
// port callback) and is applied to the Adapter on exactly one goroutine, so
// the kernel state, the outbox and the event deduplication never race.
//
// Commands are dispatched to workers. Commands for the same pool are serial;
// different pools may run concurrently. Local wallet/ledger/journal access is
// additionally serialized by BuyerPorts.StateMu.
type Loop struct {
	adapter  *Adapter
	executor *Executor

	inbox  chan loopMessage
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	busy     map[string]bool
	deferred map[string][]scheduler.Command
	retries  map[string]int
	barriers []chan struct{}

	// OnError observes asynchronous loop errors (a malformed fact, a failed
	// command that stays in the outbox).
	OnError func(error)
	// AfterEvent may return follow-up facts generated from the kernel state
	// (for example the seed -> manifest bootstrap). It runs on the loop
	// goroutine after the triggering event was applied.
	AfterEvent func(scheduler.Event, scheduler.State) []scheduler.Event
	// OnCommands observes the commands produced by one event before they are
	// dispatched. It runs on the loop goroutine and must not block.
	OnCommands func(scheduler.Command, scheduler.State)

	// MaxRetries bounds automatic re-execution of a failed command. The outbox
	// remains the cross-restart source of truth.
	MaxRetries int
}

type loopMessage struct {
	event    *scheduler.Event
	settle   string
	run      *scheduler.Command
	done     *scheduler.Command
	barrier  chan struct{}
	snapshot chan scheduler.View
	state    chan scheduler.State
}

// NewLoop creates a loop over an adapter and executor.
func NewLoop(adapter *Adapter, executor *Executor) (*Loop, error) {
	if adapter == nil || executor == nil {
		return nil, errors.New("host: adapter and executor are required")
	}
	return &Loop{
		adapter:  adapter,
		executor: executor,
		inbox:    make(chan loopMessage, 256),
		busy:     map[string]bool{},
		deferred: map[string][]scheduler.Command{},
		retries:  map[string]int{},
	}, nil
}

// Start launches the loop and re-runs every command left in the outbox.
func (l *Loop) Start(ctx context.Context) {
	l.ctx, l.cancel = context.WithCancel(ctx)
	l.wg.Add(1)
	go l.run()
	for _, entry := range l.adapter.Pending() {
		command := entry.Command
		l.enqueue(loopMessage{run: &command})
	}
}

// Stop cancels the loop and waits for its goroutine without a bound.
func (l *Loop) Stop() {
	_ = l.StopContext(context.Background())
}

// StopContext cancels the loop and waits for its goroutine until ctx is done.
// Callers should close in-flight streams first so blocked workers can exit.
func (l *Loop) StopContext(ctx context.Context) error {
	if l.cancel != nil {
		l.cancel()
	}
	done := make(chan struct{})
	go func() {
		l.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Feed enqueues one fact. It is safe to call from any goroutine, including
// port callbacks.
func (l *Loop) Feed(ctx context.Context, event scheduler.Event) error {
	if l.ctx == nil {
		return errors.New("host: loop is not started")
	}
	select {
	case l.inbox <- loopMessage{event: &event}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ctx.Done():
		return context.Canceled
	}
}

// View returns a snapshot projection from the loop goroutine. It is safe to
// poll from the entry point; the returned value owns all of its maps.
func (l *Loop) View(ctx context.Context) (scheduler.View, error) {
	if l.ctx == nil {
		return scheduler.View{}, errors.New("host: loop is not started")
	}
	response := make(chan scheduler.View, 1)
	select {
	case l.inbox <- loopMessage{snapshot: response}:
	case <-ctx.Done():
		return scheduler.View{}, ctx.Err()
	case <-l.ctx.Done():
		return scheduler.View{}, context.Canceled
	}
	select {
	case view := <-response:
		return view, nil
	case <-ctx.Done():
		return scheduler.View{}, ctx.Err()
	case <-l.ctx.Done():
		return scheduler.View{}, context.Canceled
	}
}

// WaitIdle blocks until no command is in flight and the inbox is empty. It is
// intended for tests and controlled shutdown, not for production hot paths.
func (l *Loop) WaitIdle(ctx context.Context) error {
	if l.ctx == nil {
		return errors.New("host: loop is not started")
	}
	done := make(chan struct{})
	select {
	case l.inbox <- loopMessage{barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ctx.Done():
		return context.Canceled
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-l.ctx.Done():
		return context.Canceled
	}
}

// Settle confirms a command after its execution fact was persisted. It only
// enqueues: the outbox is modified exclusively by the loop goroutine.
func (l *Loop) Settle(commandID string) error {
	if l.ctx == nil {
		return errors.New("host: loop is not started")
	}
	select {
	case l.inbox <- loopMessage{settle: commandID}:
		return nil
	case <-l.ctx.Done():
		return context.Canceled
	}
}

func (l *Loop) enqueue(message loopMessage) {
	select {
	case l.inbox <- message:
	case <-l.ctx.Done():
	}
}

func (l *Loop) report(err error) {
	if err != nil && l.OnError != nil {
		l.OnError(err)
	}
}

func (l *Loop) run() {
	defer l.wg.Done()
	for {
		select {
		case <-l.ctx.Done():
			return
		case message := <-l.inbox:
			switch {
			case message.event != nil:
				l.handleEvent(*message.event)
			case message.settle != "":
				if err := l.adapter.Settle(message.settle); err != nil {
					l.report(err)
				}
				l.executor.Forget(message.settle)
			case message.run != nil:
				l.dispatch(*message.run)
			case message.done != nil:
				l.finish(*message.done)
			case message.barrier != nil:
				l.barriers = append(l.barriers, message.barrier)
			case message.snapshot != nil:
				message.snapshot <- l.adapter.View()
			case message.state != nil:
				clone, err := scheduler.RestoreTask(l.adapter.State())
				if err != nil {
					clone = l.adapter.State()
				}
				message.state <- clone
			}
			l.signalIdle()
		}
	}
}

func (l *Loop) handleEvent(event scheduler.Event) {
	result, err := l.adapter.Handle(event)
	if err != nil {
		l.report(err)
		return
	}
	for _, command := range result.Commands {
		if l.OnCommands != nil {
			l.OnCommands(command, l.adapter.State())
		}
		l.dispatch(command)
	}
	if l.AfterEvent != nil {
		for _, followUp := range l.AfterEvent(event, l.adapter.State()) {
			if err := l.Feed(l.ctx, followUp); err != nil {
				l.report(err)
				return
			}
		}
	}
}

func (l *Loop) dispatch(command scheduler.Command) {
	key := command.PoolID
	if key != "" && l.busy[key] {
		l.deferred[key] = append(l.deferred[key], command)
		return
	}
	if key != "" {
		l.busy[key] = true
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		err := l.executor.Run(l.ctx, command)
		if err != nil {
			l.report(err)
		}
		l.enqueue(loopMessage{done: &command})
	}()
}

func (l *Loop) finish(command scheduler.Command) {
	key := command.PoolID
	if key != "" {
		if deferred := l.deferred[key]; len(deferred) > 0 {
			next := deferred[0]
			l.deferred[key] = deferred[1:]
			if len(l.deferred[key]) == 0 {
				delete(l.deferred, key)
			}
			// Release the slot before dispatching the next command: dispatch
			// would otherwise see the same pool still busy and defer it again.
			delete(l.busy, key)
			l.dispatch(next)
			return
		}
		delete(l.busy, key)
	}
	if !l.inOutbox(command.CommandID) {
		delete(l.retries, command.CommandID)
		return
	}
	// The command failed and stayed in the outbox: retry with a bounded delay.
	retries := l.retries[command.CommandID]
	maxRetries := l.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	if retries >= maxRetries {
		l.report(errors.New("host: command " + command.CommandID + " remains pending after retries"))
		return
	}
	l.retries[command.CommandID] = retries + 1
	time.AfterFunc(2*time.Second, func() {
		l.enqueue(loopMessage{run: &command})
	})
}

func (l *Loop) signalIdle() {
	if len(l.busy) != 0 || len(l.deferred) != 0 || len(l.inbox) != 0 || len(l.barriers) == 0 {
		return
	}
	for _, barrier := range l.barriers {
		close(barrier)
	}
	l.barriers = nil
}

func (l *Loop) inOutbox(commandID string) bool {
	for _, entry := range l.adapter.Pending() {
		if entry.CommandID == commandID {
			return true
		}
	}
	return false
}

// State returns a deep-copied kernel state from the loop goroutine.
func (l *Loop) State(ctx context.Context) (scheduler.State, error) {
	if l.ctx == nil {
		return scheduler.State{}, errors.New("host: loop is not started")
	}
	response := make(chan scheduler.State, 1)
	select {
	case l.inbox <- loopMessage{state: response}:
	case <-ctx.Done():
		return scheduler.State{}, ctx.Err()
	case <-l.ctx.Done():
		return scheduler.State{}, context.Canceled
	}
	select {
	case state := <-response:
		return state, nil
	case <-ctx.Done():
		return scheduler.State{}, ctx.Err()
	case <-l.ctx.Done():
		return scheduler.State{}, context.Canceled
	}
}
