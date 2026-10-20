package bitfs

import (
	"context"
	"errors"
	"sync"
)

// Admission is the shutdown boundary for inbound work: Try and Close are
// serialized, so once Close returns no new work can be registered, and Wait
// observes every unit that was admitted before the boundary.
type Admission struct {
	mu        sync.Mutex
	accepting bool
	wait      sync.WaitGroup
}

// OpenAdmission creates an accepting boundary.
func OpenAdmission() *Admission { return &Admission{accepting: true} }

// Try admits one work unit. It must be called before starting the work, and
// the caller must call Done when the whole unit (including connection setup)
// finishes.
func (a *Admission) Try() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.accepting {
		return false
	}
	a.wait.Add(1)
	return true
}

// Done marks one admitted unit as finished.
func (a *Admission) Done() { a.wait.Done() }

// Close stops further admission. Units admitted before Close are still waited
// for by Wait.
func (a *Admission) Close() {
	a.mu.Lock()
	a.accepting = false
	a.mu.Unlock()
}

// Wait closes admission and waits for admitted units until the context is done.
func (a *Admission) Wait(ctx context.Context) error {
	a.Close()
	done := make(chan struct{})
	go func() {
		a.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.New("bitfs: shutdown deadline exceeded")
	}
}
