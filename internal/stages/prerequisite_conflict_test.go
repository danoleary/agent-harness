package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
)

var designPR = ticket.Prerequisite{Identifier: "#81", Title: "Design record", Branch: "feat/gh-80"}

// #82: the review fetched the ticket while its prerequisite PR was still open, the
// PR opened, and then that prerequisite merged — so a conflict with base the
// reactive rebase cannot replay is the prerequisite's merge. Rather than park a
// cold-reviewed, gate-green PR for a manual rebase, the watch is offered one
// conflict-resolution session plus a host gate.
func TestReviewCIWatchResolvesAConflictFromAMergedPrerequisiteOnce(t *testing.T) {
	h, _ := handedOff(t, "PROJ-82")
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
		h, _ := handedOff(t, "PROJ-83")
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

// #344: the CI watch's reactive rebase runs after the PR opened, so the review's
// own `## Overlap with main` section is out of reach (ADR-0050). A replay there
// that reports the branch duplicates what main added puts the report on the open
// PR's description, kept otherwise as the landing opened it.
func TestReviewCIWatchPutsAReactiveReplaysOverlapReportOnTheOpenPR(t *testing.T) {
	h, _ := handedOff(t, "PROJ-84")
	h.Trk.Ticket.Prerequisites = []ticket.Prerequisite{designPR}
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			report := `[{"file": "internal/hostio/hostio.go", "main_added": "Host.HandToHuman", "branch_duplicates": "Host.Escalate"}]`
			if err := os.WriteFile(filepath.Join(run.FindingsDir, prompt.OverlapsFile), []byte(report), 0o644); err != nil {
				t.Fatal(err)
			}
			return session.Outcome{}
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		h.Trk.Ticket.Prerequisites = nil // #81 merged after the PR opened
		if !w.Resolve() {
			t.Fatal("a conflict after the prerequisite merged must be resolved")
		}
		return ci.Outcome{OK: true}
	}

	Review(h, stageCfg(), stageLog(t, "PROJ-84"), Args{Identifier: "PROJ-84"})

	if len(h.PREdits) != 1 || len(h.PRBodies) != 1 {
		t.Fatalf("want one PR, edited once with the overlap report; bodies %v, edits %+v", h.PRBodies, h.PREdits)
	}
	edit, opened := h.PREdits[0], h.PRBodies[0]
	if want := "\n- `internal/hostio/hostio.go`: `main` added Host.HandToHuman; the branch duplicates it with Host.Escalate."; !strings.Contains(edit.Body, want) {
		t.Errorf("PR body lacks %q:\n%s", want, edit.Body)
	}
	if !strings.Contains(edit.Body, "## Overlap with `main`") {
		t.Errorf("PR body lacks the overlap section:\n%s", edit.Body)
	}
	head, trailer, _ := strings.Cut(opened, "\n---\n")
	if !strings.HasPrefix(edit.Body, strings.TrimRight(head, "\n")) || !strings.HasSuffix(edit.Body, "\n---\n"+trailer) {
		t.Errorf("the edit keeps the description the PR opened with:\nopened:\n%s\nedited:\n%s", opened, edit.Body)
	}
	if edit.Number != 1 {
		t.Errorf("edited PR #%d, want #1, the one the landing opened", edit.Number)
	}
}
