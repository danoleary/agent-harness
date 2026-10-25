package jira

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/lease"
)

// issueStatusPath is the GET StillReady reads the issue's current status with.
func issueStatusPath(key string) string { return "GET /rest/api/2/issue/" + key + "?fields=status" }

// startProgress lists the claim transition, leading to status to.
func startProgress(to string) map[string]any {
	return map[string]any{"transitions": []any{
		map[string]any{"id": "11", "name": "Back to To Do", "to": map[string]any{"id": "1"}},
		map[string]any{"id": "21", "name": "Start Progress", "to": map[string]any{"id": to}},
	}}
}

// StillReady asks the one issue whether the claim transition would still move
// it: offered, and leading somewhere other than the status it is in. A claimed
// issue no longer offers it, or (with a global transition) is already where it
// leads (#89).
func TestStillReadyReadsTheIssuesTransitions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      string
		transitions map[string]any
		want        bool
	}{
		{"ready", "1", startProgress("3"), true},
		{"claimed, global transition", "3", startProgress("3"), false},
		{"claimed, transition gone", "3", map[string]any{"transitions": []any{
			map[string]any{"id": "11", "name": "Back to To Do", "to": map[string]any{"id": "1"}},
		}}, false},
	} {
		tr, _ := fakeTransport(map[string]any{
			transitionsPath("PROJ-9"): tc.transitions,
			issueStatusPath("PROJ-9"): map[string]any{"key": "PROJ-9", "fields": map[string]any{"status": map[string]any{"id": tc.status}}},
		})
		got, err := NewClient(tr, baseURL, selectOpts).StillReady("PROJ-9")
		if err != nil || got != tc.want {
			t.Errorf("%s: StillReady() = %v, %v; want %v, nil", tc.name, got, err, tc.want)
		}
	}
}

// jiraBoard is one Jira issue as two instances see it: a JQL search that lags
// (the enhanced search is eventually consistent, so it still returns PROJ-9
// after a claim), the issue's own status, and the transition that claims it.
type jiraBoard struct {
	mu     sync.Mutex
	status string
	claims int
}

func (b *jiraBoard) transport(method, path string, body any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var resp any = map[string]any{}
	switch method + " " + path {
	case selectPath:
		resp = map[string]any{"issues": []any{map[string]any{"key": "PROJ-9", "fields": map[string]any{"summary": "t"}}}}
	case transitionsPath("PROJ-9"):
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond) // widen the window two unexcluded claims would race in
		b.mu.Lock()
		if b.status == "1" {
			resp = startProgress("3")
		} else {
			resp = map[string]any{"transitions": []any{}}
		}
	case issueStatusPath("PROJ-9"):
		resp = map[string]any{"key": "PROJ-9", "fields": map[string]any{"status": map[string]any{"id": b.status}}}
	case "POST /rest/api/2/issue/PROJ-9/transitions":
		b.claims++
		b.status = "3"
	default:
		return nil, fmt.Errorf("unexpected transport call: %s %s", method, path)
	}
	return json.Marshal(resp)
}

// mutexLock is the host's claim lock, in memory.
type mutexLock struct{ sync.Mutex }

func (m *mutexLock) Lock() (func(), error) {
	m.Mutex.Lock()
	return m.Mutex.Unlock, nil
}

// Two instances select at the same moment against one Jira project and both see
// PROJ-9 first (#89). Exactly one proceeds on it; the other's claim is lost.
func TestTwoInstancesNeverBothClaimOneJiraIssue(t *testing.T) {
	b := &jiraBoard{status: "1"}
	lock := &mutexLock{}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := NewClient(b.transport, baseURL, selectOpts)
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
	if winners.Load() != 1 || b.claims != 1 {
		t.Errorf("%d instances proceeded on PROJ-9 over %d claims, want exactly 1 of each", winners.Load(), b.claims)
	}
}
