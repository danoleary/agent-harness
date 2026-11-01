package loop

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/stages"
)

// prSource is a Source whose items are not tracker tickets: open pull requests,
// keyed by PR number and claimed with a label, the shape ADR-0019's second loop
// takes. It exists to prove the seam is real — the daemon works it without a
// tracker anywhere in sight.
type prSource struct {
	open     []int
	labelled []string // the claim: a PR carries the label while a run holds it
	comments map[string][]string
	stale    []StaleClaim
}

func (s *prSource) MoveToInProgress(key string) error {
	s.labelled = append(s.labelled, key)
	return nil
}

func (s *prSource) ReleaseToTodo(key string) error {
	for i, k := range s.labelled {
		if k == key {
			s.labelled = append(s.labelled[:i], s.labelled[i+1:]...)
			break
		}
	}
	return nil
}

func (s *prSource) MoveToCanceled(key string) error { return s.ReleaseToTodo(key) }

func (s *prSource) Next() (WorkItem, *lease.Lease, bool, error) {
	if len(s.open) == 0 {
		return WorkItem{}, nil, false, nil
	}
	n := s.open[0]
	s.open = s.open[1:]
	key := fmt.Sprintf("PR-%d", n)
	claim := lease.Unheld(s, key)
	if err := claim.Hold(); err != nil {
		return WorkItem{}, nil, false, err
	}
	return WorkItem{Key: key, Branch: "someone/their-branch-" + key}, claim, true, nil
}

func (s *prSource) Claims() ([]StaleClaim, error) { return s.stale, nil }
func (s *prSource) HasRemoteBranch(string) bool   { return false }
func (s *prSource) Release(key string) error      { return s.ReleaseToTodo(key) }

// Releasing words a give-back as a PR loop would: there is no Todo, and the lease
// is a label.
func (s *prSource) Releasing(why ReleaseReason, key string) string {
	switch why {
	case ReleaseNoPR:
		return key + " pushed nothing; releasing its label"
	case ReleaseCapAbort:
		return "spending-cap abort on " + key + "; releasing its label"
	default:
		return "reaping " + key + "'s stale label"
	}
}

func (s *prSource) Comment(key, body string) error {
	if s.comments == nil {
		s.comments = map[string][]string{}
	}
	s.comments[key] = append(s.comments[key], body)
	return nil
}

// prPipeline records the items it was handed, and fails to ship the ones listed in
// noPR.
type prPipeline struct {
	ran  []WorkItem
	noPR map[string]bool
}

func (p *prPipeline) Run(item WorkItem, _ *lease.Lease) stages.Result {
	p.ran = append(p.ran, item)
	if p.noPR[item.Key] {
		return stages.Result{Disposition: stages.NoPR}
	}
	return stages.Result{OK: true, Disposition: stages.Shipped}
}

func (p *prPipeline) Recover(WorkItem) (stages.Result, bool) { return stages.Result{}, false }

// The daemon works whatever its Source yields: items that are PRs, not tickets,
// reach the Pipeline whole — the Source's own Branch included — and a run that
// fails to ship gives its claim back through the Source's lease and leaves the
// breadcrumb on the item, with no tracker involved (ADR-0019).
func TestDaemonWorksASourceWhoseItemsAreNotTickets(t *testing.T) {
	r := &recorder{}
	src := &prSource{open: []int{3, 7}}
	pipe := &prPipeline{noPR: map[string]bool{"PR-7": true}}

	code := Run(Deps{
		Host:     &fakeHost{stopRequested: stopAfter(2)},
		Source:   src,
		Pipeline: pipe,
		Limits:   Limits{PollInterval: time.Minute, TickInterval: 2 * time.Second, MaxConsecutiveFailures: 3},
		Clock:    testClock{},
		Log:      r,
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	want := []WorkItem{
		{Key: "PR-3", Branch: "someone/their-branch-PR-3"},
		{Key: "PR-7", Branch: "someone/their-branch-PR-7"},
	}
	if !reflect.DeepEqual(pipe.ran, want) {
		t.Errorf("pipeline ran %v, want %v (each item reaches the Pipeline as the Source made it)", pipe.ran, want)
	}
	if want := []string{"PR-3"}; !reflect.DeepEqual(src.labelled, want) {
		t.Errorf("labelled = %v, want %v (the shipped PR keeps its claim; the no-PR one is released)", src.labelled, want)
	}
	if len(src.comments["PR-7"]) != 1 || len(src.comments["PR-3"]) != 0 {
		t.Errorf("comments = %v, want one breadcrumb on PR-7 only", src.comments)
	}
}

// #173: on herd PR-1312 (#167) a PR loop attempt pushed nothing, and the daemon
// narrated it as "PR-1312 produced no PR; releasing back to Todo so it isn't
// stranded In Progress" — but a PR has no Todo, its run never meant to produce a
// PR, and its lease is a label. Every line the daemon writes as it gives an item
// back — a run that shipped nothing, a spending-cap abort, a reaped stale claim —
// is the Source's own wording, so no PR is ever narrated in ticket words.
func TestDaemonNarratesAGiveBackInTheSourcesWords(t *testing.T) {
	stale := []StaleClaim{{Key: "PR-1312", StartedAt: fixedNow.Add(-2 * time.Hour)}}
	for _, tc := range []struct {
		name  string
		why   ReleaseReason
		open  []int
		stale []StaleClaim
		run   stages.Disposition
	}{
		{name: "a run that pushed nothing", why: ReleaseNoPR, open: []int{1312}, run: stages.NoPR},
		{name: "a spending-cap abort", why: ReleaseCapAbort, open: []int{1312}, run: stages.CapAborted},
		{name: "a reaped stale claim", why: ReleaseStale, stale: stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{}
			src := &prSource{open: tc.open, stale: tc.stale}
			Run(Deps{
				Host:   &fakeHost{stopRequested: stopAfter(1)},
				Source: src,
				Pipeline: &fakePipeline{run: func(string) stages.Result {
					return stages.Result{Disposition: tc.run}
				}},
				Limits: Limits{ClaimTTL: 30 * time.Minute, PollInterval: time.Minute, TickInterval: 2 * time.Second, CapBackoff: time.Minute, MaxConsecutiveFailures: 3},
				Clock:  testClock{now: func() time.Time { return fixedNow }, sleep: func(time.Duration) {}},
				Log:    r,
			})

			want := "loop — " + src.Releasing(tc.why, "PR-1312")
			if !r.saw(want) {
				t.Errorf("no record %q; events = %v", want, r.events)
			}
			for _, e := range r.events {
				if strings.Contains(e, "Todo") || strings.Contains(e, "In Progress") {
					t.Errorf("event %q narrates a PR in ticket words", e)
				}
			}
		})
	}
}
