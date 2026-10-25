package linear

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/lease"
)

// stateTransport answers the state read with one issue's state type.
func stateTransport(t *testing.T, issue any) Transport {
	t.Helper()
	tr, _ := transportReturning(t, map[string]any{"issue": issue})
	return tr
}

// StillReady reads the one ticket's state: an unstarted ticket is still
// claimable, a started one is another run's (#89), and a missing one is an
// error rather than a guess.
func TestStillReadyReadsTheTicketsState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		issue any
		want  bool
	}{
		{"unstarted", map[string]any{"state": map[string]any{"type": "unstarted"}}, true},
		{"started", map[string]any{"state": map[string]any{"type": "started"}}, false},
		{"canceled", map[string]any{"state": map[string]any{"type": "canceled"}}, false},
	} {
		got, err := NewClient(stateTransport(t, tc.issue), testOptions()).StillReady("BEH-1")
		if err != nil || got != tc.want {
			t.Errorf("%s: StillReady() = %v, %v; want %v, nil", tc.name, got, err, tc.want)
		}
	}
	if _, err := NewClient(stateTransport(t, nil), testOptions()).StillReady("BEH-1"); err == nil {
		t.Error("a ticket Linear cannot find must be an error, not a ready or claimed guess")
	}
}

// linearBoard is one Linear team as two instances see it: a selection read that
// lags (it returns the ticket even after a claim, as a stale page would), the
// ticket's own state, and the move that claims it.
type linearBoard struct {
	mu    sync.Mutex
	state string
	moves int
}

func (b *linearBoard) transport(query string, vars map[string]any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var resp any
	switch {
	case query == selectNextQuery:
		resp = map[string]any{"issues": map[string]any{"nodes": []any{map[string]any{
			"identifier": "BEH-7", "title": "t", "priority": 1,
			"labels": map[string]any{"nodes": []any{map[string]any{"name": "ready-for-agent"}}},
		}}}}
	case query == issueStateQuery:
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond) // widen the window two unexcluded claims would race in
		b.mu.Lock()
		resp = map[string]any{"issue": map[string]any{"state": map[string]any{"type": b.state}}}
	case query == fetchIssueStatesQuery:
		resp = map[string]any{"issue": map[string]any{"id": "uuid-7", "team": map[string]any{"states": map[string]any{"nodes": []any{
			map[string]any{"id": "s-todo", "name": "Todo", "type": "unstarted"},
			map[string]any{"id": "s-ip", "name": "In Progress", "type": "started"},
		}}}}}
	case strings.Contains(query, "issueUpdate"):
		b.moves++
		if vars["stateId"] == "s-ip" {
			b.state = "started"
		}
		resp = map[string]any{"issueUpdate": map[string]any{"success": true}}
	default:
		resp = map[string]any{}
	}
	return json.Marshal(resp)
}

// mutexLock is the host's claim lock, in memory.
type mutexLock struct{ sync.Mutex }

func (m *mutexLock) Lock() (func(), error) {
	m.Mutex.Lock()
	return m.Mutex.Unlock, nil
}

// Two instances select at the same moment against one Linear team and both see
// BEH-7 at the top (#89). Exactly one proceeds on it; the other's claim is lost.
func TestTwoInstancesNeverBothClaimOneLinearTicket(t *testing.T) {
	b := &linearBoard{state: "unstarted"}
	lock := &mutexLock{}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := NewClient(b.transport, testOptions())
			tk, ok, err := c.SelectNextTicket()
			if err != nil || !ok {
				t.Errorf("SelectNextTicket() = %v, %v", ok, err)
				return
			}
			_, won, err := lease.Claim(c, lock, tk.Identifier)
			if err != nil {
				t.Errorf("Claim() = %v", err)
			}
			if won {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 || b.moves != 1 {
		t.Errorf("%d instances proceeded on BEH-7 over %d moves, want exactly 1 of each", winners.Load(), b.moves)
	}
}
