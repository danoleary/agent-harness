// Package lease owns one run's claim on one ticket (#25). A claimed ticket sits In
// Progress; the run either keeps it (a shipped PR), returns it to Todo so a later
// run can re-grab it, or closes it as a no-op. Before the lease that lifecycle was
// decided in five modules with a "pre-claimed" flag threaded between them, and the
// flag went stale: a stage released the ticket, the pipeline retried, and the
// retry skipped its claim because selection had "already claimed" it.
//
// The lease remembers whether the ticket is held right now, so every caller asks
// for the state it needs — Hold before work, Release after a run that made no
// progress — and the lease issues only the tracker mutations that change it.
package lease

import "github.com/danoleary/agent-harness/internal/tracker"

// Queue is the slice of the Tracker port a lease drives (ADR-0010 layer 2).
type Queue interface {
	MoveToInProgress(key tracker.Key) error
	ReleaseToTodo(key tracker.Key) error
	MoveToCanceled(key tracker.Key) error
}

// Lease is one run's claim on one ticket. It is not safe for concurrent use: a
// run works its ticket from one goroutine.
type Lease struct {
	q    Queue
	key  tracker.Key
	held bool
}

// Held returns a lease on a ticket already claimed — by selection, which claims
// on dequeue (ADR-0003).
func Held(q Queue, key tracker.Key) *Lease { return &Lease{q: q, key: key, held: true} }

// Unheld returns a lease on a hand-passed ticket nothing has claimed yet. The
// implementation stage claims it after the Docker preflight, so a host that cannot
// launch a sandbox never strands the ticket In Progress (BEH-316).
func Unheld(q Queue, key tracker.Key) *Lease { return &Lease{q: q, key: key} }

// Key is the ticket the lease is on.
func (l *Lease) Key() tracker.Key { return l.key }

// Held reports whether the ticket is claimed In Progress by this run right now.
func (l *Lease) Held() bool { return l.held }

// Hold claims the ticket unless the lease already holds it.
func (l *Lease) Hold() error {
	if l.held {
		return nil
	}
	if err := l.q.MoveToInProgress(l.key); err != nil {
		return err
	}
	l.held = true
	return nil
}

// Release returns a held ticket to Todo so a later run can re-grab it. Releasing a
// ticket the lease does not hold is a no-op, so a stage and the loop can both
// settle the same failed run without a second tracker mutation.
func (l *Lease) Release() error {
	if !l.held {
		return nil
	}
	if err := l.q.ReleaseToTodo(l.key); err != nil {
		return err
	}
	l.held = false
	return nil
}

// Close moves the ticket to the tracker's terminal canceled state: the run found it
// a duplicate/superseded no-op (BEH-682). The lease no longer holds it, so a later
// Release cannot return the closed ticket to the queue.
func (l *Lease) Close() error {
	if err := l.q.MoveToCanceled(l.key); err != nil {
		return err
	}
	l.held = false
	return nil
}
