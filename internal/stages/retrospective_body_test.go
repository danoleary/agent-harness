package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// The retrospective's body was 11.5% covered, and the one test that reached it
// worked only because the precondition returned before the first Docker call — its
// own comment said so. With the hostio.Host seam the rest of the body is reachable.

// writesDropbox scripts a session that writes the findings dropbox, which is the
// stage's ground truth that it ran at all.
func writesDropbox(body string) func(int, hostio.AgentRun) session.Outcome {
	return func(_ int, run hostio.AgentRun) session.Outcome {
		_ = os.WriteFile(filepath.Join(run.FindingsDir, "out.json"), []byte(body), 0o644)
		return session.Outcome{}
	}
}

// The happy path: the session writes its dropbox, the findings are routed by
// audience, and — because the branch reached a PR — the worktree is reaped.
func TestRetrospectiveRoutesFindingsAndReapsTheWorktree(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = writesDropbox("[]")

	res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})

	if !res.OK {
		t.Fatalf("a present dropbox is the ground truth that it ran, got %+v", res)
	}
	if got := h.Routed; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("routed = %v, want the dropbox routed by audience (ADR-0011)", got)
	}
	if got := h.Removed; len(got) != 1 || got[0] != "proj-1" {
		t.Errorf("removed = %v, want the worktree reaped once the work escaped it", got)
	}
}

// An empty `[]` is a valid "ran, found nothing" — an ABSENT file is what means the
// step never ran (DESIGN.md "Success is ground-truth").
func TestRetrospectiveAbsentDropboxIsNeverRan(t *testing.T) {
	h := hostio.NewFake()
	log := stageLog(t, "PROJ-2")

	res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-2"})

	if res.OK {
		t.Fatal("an absent dropbox means the retrospective never ran")
	}
	if !strings.Contains(narration(t, log), "never ran") {
		t.Errorf("run.jsonl = %s, want the never-ran verdict", narration(t, log))
	}
	if len(h.Removed) != 0 {
		t.Errorf("a failed retrospective keeps the worktree as a breadcrumb, got %v", h.Removed)
	}
}

// BEH-568: a cap abort can fire after the skill wrote its up-front default `[]`.
// Left in place that reads as "ran, found nothing", masking the abort. The empty
// default is dropped so the "absent file = never ran" contract holds and the stage
// reports retry-after-reset.
func TestRetrospectiveCapAbortClearsTheDefaultEmptyDropbox(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		_ = os.WriteFile(filepath.Join(run.FindingsDir, "out.json"), []byte("[]"), 0o644)
		return session.Outcome{ExitCode: 1, SpendingCapAbort: true}
	}
	log := stageLog(t, "PROJ-3")

	res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-3"})

	if res.OK || res.Disposition != CapAborted {
		t.Fatalf("a capped retrospective is retry-after-reset, got %+v", res)
	}
	if !strings.Contains(narration(t, log), "spending cap") {
		t.Errorf("run.jsonl = %s, want the cap abort named", narration(t, log))
	}
}

// BEH-536: a 137 kill that struck after the read-heavy analysis but before the
// write gets the retry class (↻), not the misleading generic "never ran" (✗).
func TestRetrospectiveOOMKillIsARetryClassNotANeverRan(t *testing.T) {
	h := hostio.NewFake()
	h.AgentOutcome = session.Outcome{ExitCode: sandbox.ExitOOMKill}
	log := stageLog(t, "PROJ-4")

	if res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-4"}); res.OK {
		t.Fatal("a killed retrospective wrote nothing")
	}
	if !strings.Contains(narration(t, log), "retrospective ↻") {
		t.Errorf("run.jsonl = %s, want the ↻ retry class (BEH-536)", narration(t, log))
	}
}

// The worktree is reaped only once the work has escaped it: an unpushed branch
// keeps it, and the gh round-trip is never spent (&& short-circuits).
func TestRetrospectiveKeepsTheWorktreeWhenTheBranchNeverShipped(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = writesDropbox("[]")
	h.Pushed = false

	res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-5"), Args{Identifier: "PROJ-5"})

	if !res.OK {
		t.Fatalf("the retrospective itself succeeded, got %+v", res)
	}
	if len(h.Removed) != 0 {
		t.Errorf("an unpushed branch keeps its worktree, got %v", h.Removed)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "pr-exists") {
		t.Error("an unpushed branch must never spend the gh round-trip")
	}
}

// BEH-539: the finding classes a prior run already filed are passed to the session
// as settled context — gathered BEFORE ClearDropbox wipes that prior dropbox.
func TestRetrospectivePassesAlreadyFiledFindingsAsSettledContext(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = writesDropbox("[]")
	h.Prior = priorFindings()
	log := stageLog(t, "PROJ-6")

	Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-6"})

	if !strings.Contains(narration(t, log), "already-filed finding class") {
		t.Errorf("run.jsonl = %s, want the settled classes narrated (BEH-539)", narration(t, log))
	}
	if len(h.Agents) != 1 || !strings.Contains(h.Agents[0].Prompt, "sandbox-playwright-missing-deps") {
		t.Error("the prior finding classes must reach the session prompt as settled context")
	}
}

// --dry-run launches nothing.
func TestRetrospectiveDryRunLaunchesNothing(t *testing.T) {
	h := hostio.NewFake()

	if res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-7"), Args{Identifier: "PROJ-7", DryRun: true}); !res.OK {
		t.Fatalf("a dry run is a success, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("a dry run launches nothing, got %v", labels(h.Agents))
	}
}

// priorFindings is one already-filed finding class, as a prior run would have left it.
func priorFindings() []filing.PriorFinding {
	return []filing.PriorFinding{{Key: "sandbox-playwright-missing-deps", Title: "Playwright deps missing in the sandbox"}}
}
