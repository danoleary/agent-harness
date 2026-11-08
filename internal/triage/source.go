package triage

import (
	"fmt"

	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// Source is the `backlog` [loop.Source]: the tracker's untriaged tickets that have
// no verdict recorded at their current fingerprint, one per poll.
//
// Triage claims nothing. It changes a ticket only to mark it ready or to comment
// on it, so its lease is never held, and there is nothing for the reaper to find
// or the daemon to release.
type Source struct {
	backlog Backlog
	store   Store
}

var _ loop.Source = (*Source)(nil)

// NewSource is the Source over a tracker's backlog and the recorded verdicts.
func NewSource(b Backlog, s Store) *Source {
	return &Source{backlog: b, store: s}
}

// CheckAccess proves the tracker answers the backlog read, once at startup.
func (s *Source) CheckAccess() error {
	if _, err := s.backlog.Untriaged(); err != nil {
		return fmt.Errorf("could not list the tracker's untriaged tickets: %w", err)
	}
	return nil
}

// Plan lists the tickets the Source would yield, in order, without changing
// anything: what `loop triage --dry-run` prints.
func (s *Source) Plan() ([]Item, error) {
	tickets, err := s.backlog.Untriaged()
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, t := range tickets {
		fp := Fingerprint(t)
		if s.store.Judged(t.Identifier, fp) {
			continue
		}
		out = append(out, Item{Ticket: t, Fingerprint: fp})
	}
	return out, nil
}

// Next yields the first ticket with no verdict at its current fingerprint.
func (s *Source) Next() (loop.WorkItem, *lease.Lease, bool, error) {
	plan, err := s.Plan()
	if err != nil || len(plan) == 0 {
		return loop.WorkItem{}, nil, false, err
	}
	it := plan[0]
	key := it.Ticket.Identifier
	return loop.WorkItem{Key: key, Context: it}, lease.Unheld(noClaim{}, key), true, nil
}

// Claims is empty: triage holds no claim.
func (s *Source) Claims() ([]loop.StaleClaim, error) { return nil, nil }

// HasRemoteBranch is never asked, as there are no claims to reap; it fails safe.
func (s *Source) HasRemoteBranch(string) bool { return true }

// Release has nothing to give back.
func (s *Source) Release(string) error { return nil }

// Comment posts nothing: the daemon's breadcrumbs are worded for a claimed ticket
// ("released back to Todo"), which a triaged ticket never was. A run that reached
// no verdict leaves the ticket as it was, and the next poll judges it again.
func (s *Source) Comment(string, string) error { return nil }

// Block is never asked: the triage Pipeline recovers nothing.
func (s *Source) Block(string) error { return nil }

// Releasing words the daemon's line for a run that reached no verdict.
func (s *Source) Releasing(why loop.ReleaseReason, key string) string {
	switch why {
	case loop.ReleaseCapAbort:
		return "spending-cap abort while triaging " + key + "; it is left as it was and judged again after the backoff"
	default:
		return key + " reached no triage verdict; it is left as it was and judged again on a later poll"
	}
}

// noClaim is the lease queue of a Source that claims nothing. An unheld lease
// never calls it; it exists so a stray Hold could not change the tracker.
type noClaim struct{}

func (noClaim) MoveToInProgress(tracker.Key) error { return nil }
func (noClaim) ReleaseToTodo(tracker.Key) error    { return nil }
func (noClaim) MoveToCanceled(tracker.Key) error   { return nil }
