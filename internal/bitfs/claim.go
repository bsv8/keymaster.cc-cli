package bitfs

import "sync/atomic"

// Claim guards one download task: exactly one inbound seller session may own
// the purchase and the pool ledger at a time. Additional sessions are declined
// instead of running a second purchase over the same funds.
type Claim struct {
	claimed atomic.Bool
}

// Try claims the task and reports whether this caller owns it.
func (c *Claim) Try() bool { return c.claimed.CompareAndSwap(false, true) }

// Held reports whether the task is already claimed.
func (c *Claim) Held() bool { return c.claimed.Load() }

// Release returns the claim (used by tests and future task-level scheduling).
func (c *Claim) Release() { c.claimed.Store(false) }
