package loop

import (
	"fmt"
	"reflect"
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
	key := fmt.Sprintf("pr-%d", n)
	claim := lease.Unheld(s, key)
	if err := claim.Hold(); err != nil {
		return WorkItem{}, nil, false, err
	}
	return WorkItem{Key: key, Branch: "someone/their-branch-" + key}, claim, true, nil
}

func (s *prSource) Claims() ([]StaleClaim, error) { return nil, nil }
func (s *prSource) HasRemoteBranch(string) bool   { return true }
func (s *prSource) Release(key string) error      { return s.ReleaseToTodo(key) }
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
	pipe := &prPipeline{noPR: map[string]bool{"pr-7": true}}

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
		{Key: "pr-3", Branch: "someone/their-branch-pr-3"},
		{Key: "pr-7", Branch: "someone/their-branch-pr-7"},
	}
	if !reflect.DeepEqual(pipe.ran, want) {
		t.Errorf("pipeline ran %v, want %v (each item reaches the Pipeline as the Source made it)", pipe.ran, want)
	}
	if want := []string{"pr-3"}; !reflect.DeepEqual(src.labelled, want) {
		t.Errorf("labelled = %v, want %v (the shipped PR keeps its claim; the no-PR one is released)", src.labelled, want)
	}
	if len(src.comments["pr-7"]) != 1 || len(src.comments["pr-3"]) != 0 {
		t.Errorf("comments = %v, want one breadcrumb on pr-7 only", src.comments)
	}
}
