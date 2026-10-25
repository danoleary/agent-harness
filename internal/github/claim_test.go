package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/lease"
)

// StillReady reads the one issue: open, still carrying the ready label and not
// yet the in-progress one. Anything else is another run's claim, or a ticket a
// human has taken off the queue (#89).
func TestStillReadyReadsTheIssuesLabels(t *testing.T) {
	label := func(names ...string) []any {
		var out []any
		for _, n := range names {
			out = append(out, map[string]any{"name": n})
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		issue map[string]any
		want  bool
	}{
		{"ready", map[string]any{"number": 9, "state": "open", "labels": label("ready-for-agent")}, true},
		{"claimed", map[string]any{"number": 9, "state": "open", "labels": label("in-progress")}, false},
		{"mid-claim", map[string]any{"number": 9, "state": "open", "labels": label("ready-for-agent", "in-progress")}, false},
		{"closed", map[string]any{"number": 9, "state": "closed", "labels": label("ready-for-agent")}, false},
	} {
		tr, calls := fakeTransport(map[string]any{"GET /repos/acme/widgets/issues/9": tc.issue})
		got, err := NewClient(tr, "acme", "widgets", readyLabels).StillReady("#9")
		if err != nil || got != tc.want {
			t.Errorf("%s: StillReady() = %v, %v; want %v, nil", tc.name, got, err, tc.want)
		}
		if len(*calls) != 1 {
			t.Errorf("%s: StillReady made %d calls, want the one issue read", tc.name, len(*calls))
		}
	}
}

// githubBoard is one repo's issue #9 as two instances see it: a list read that
// lags (it still returns #9 after a claim), the issue's own labels, and the
// label edits that claim it.
type githubBoard struct {
	mu     sync.Mutex
	labels map[string]bool
	claims int
}

func (b *githubBoard) transport(method, path string, body any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var resp any = map[string]any{}
	switch {
	case method+" "+path == selectPath:
		resp = []any{map[string]any{"number": 9, "title": "t", "labels": []any{map[string]any{"name": "ready-for-agent"}}}}
	case method == "GET" && path == "/repos/acme/widgets/issues/9":
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond) // widen the window two unexcluded claims would race in
		b.mu.Lock()
		var labels []any
		for name, on := range b.labels {
			if on {
				labels = append(labels, map[string]any{"name": name})
			}
		}
		resp = map[string]any{"number": 9, "state": "open", "labels": labels}
	case method == "POST" && path == "/repos/acme/widgets/issues/9/labels":
		for _, l := range labelsInBody(body) {
			if l == "in-progress" {
				b.claims++
			}
			b.labels[l] = true
		}
	case method == "DELETE" && strings.HasPrefix(path, "/repos/acme/widgets/issues/9/labels/"):
		b.labels[strings.TrimPrefix(path, "/repos/acme/widgets/issues/9/labels/")] = false
	case method == "POST" && path == "/repos/acme/widgets/issues/9/assignees":
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

// Two instances select at the same moment against one repo and both see #9 at
// the top (#89). Exactly one proceeds on it; the other's claim is lost.
func TestTwoInstancesNeverBothClaimOneGitHubIssue(t *testing.T) {
	b := &githubBoard{labels: map[string]bool{"ready-for-agent": true}}
	lock := &mutexLock{}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := NewClient(b.transport, "acme", "widgets", readyLabels)
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
		t.Errorf("%d instances proceeded on #9 over %d claims, want exactly 1 of each", winners.Load(), b.claims)
	}
}
