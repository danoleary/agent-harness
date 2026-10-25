package stages

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/ticket"
)

var designPR = ticket.Prerequisite{Identifier: "#81", Title: "Design record", Branch: "feat/gh-80"}

// #82: the review fetched the ticket while its prerequisite PR was still open, the
// PR opened, and then that prerequisite merged — so a conflict with base the
// reactive rebase cannot replay is the prerequisite's merge. Rather than park a
// cold-reviewed, gate-green PR for a manual rebase, the watch is offered one
// conflict-resolution session plus a host gate.
func TestReviewCIWatchResolvesAConflictFromAMergedPrerequisiteOnce(t *testing.T) {
	h := hostio.NewFake()
	h.Trk.Ticket.Prerequisites = []ticket.Prerequisite{designPR}
	var first, second bool
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		h.Trk.Ticket.Prerequisites = nil // #81 merged after the PR opened
		first = w.Resolve != nil && w.Resolve()
		second = w.Resolve()
		return ci.Outcome{OK: true}
	}

	Review(h, stageCfg(), stageLog(t, "PROJ-82"), Args{Identifier: "PROJ-82"})

	if !first {
		t.Fatal("a conflict after the prerequisite merged must be resolved")
	}
	if second {
		t.Error("the resolution is offered once per watch, never a loop")
	}
	if got := labels(h.Agents); len(got) != 2 || got[1] != "rebasefix" {
		t.Errorf("agent runs = %v, want one conflict-resolution session after the review", got)
	}
	if got := shellLabels(h.Shells); got[len(got)-1] != gateStep+"-cirebase-check" {
		t.Errorf("shell runs = %v, want the host gate re-run over the resolved tree", got)
	}
}

// A conflict that is NOT the prerequisite's merge — the prerequisite is still
// open, or there never was one — keeps today's behaviour: park for a human.
func TestReviewCIWatchLeavesOtherConflictsForAHuman(t *testing.T) {
	for name, prereqs := range map[string][]ticket.Prerequisite{"still open": {designPR}, "none": nil} {
		h := hostio.NewFake()
		h.Trk.Ticket.Prerequisites = prereqs
		resolved := true
		h.CIFn = func(w hostio.CIWatch) ci.Outcome {
			resolved = w.Resolve != nil && w.Resolve()
			return ci.Outcome{OK: true}
		}

		Review(h, stageCfg(), stageLog(t, "PROJ-83"), Args{Identifier: "PROJ-83"})

		if resolved {
			t.Errorf("%s: only a merged prerequisite earns a resolution session", name)
		}
		if strings.Contains(strings.Join(labels(h.Agents), "|"), "rebasefix") {
			t.Errorf("%s: no resolution session may run, got %v", name, labels(h.Agents))
		}
	}
}
