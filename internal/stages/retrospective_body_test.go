package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
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

// shipped is a host for key whose handoff reached origin with its PR open: the
// state a green review leaves the retrospective.
func shipped(t *testing.T, key string) *hostio.Fake {
	t.Helper()
	h, _ := handedOff(t, key)
	if err := h.PushForceWithLease(); err != nil {
		t.Fatalf("push: %v", err)
	}
	h.Calls = nil
	return h
}

// The happy path: the session writes its dropbox, the findings are routed by
// audience, and — because the branch reached a PR — the worktree is reaped.
func TestRetrospectiveRoutesFindingsAndReapsTheWorktree(t *testing.T) {
	h := shipped(t, "PROJ-1")
	h.AgentFn = writesDropbox("[]")

	res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})

	if !res.OK {
		t.Fatalf("a present dropbox is the ground truth that it ran, got %+v", res)
	}
	if got := h.Routed; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("routed = %v, want the dropbox routed by audience (ADR-0011)", got)
	}
	if h.Worktree().Exists() {
		t.Error("the worktree must be reaped once the work escaped it")
	}
}

// An empty `[]` is a valid "ran, found nothing" — an ABSENT file is what means the
// step never ran (DESIGN.md "Success is ground-truth").
func TestRetrospectiveAbsentDropboxIsNeverRan(t *testing.T) {
	h := shipped(t, "PROJ-2")
	log := stageLog(t, "PROJ-2")

	res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-2"})

	if res.OK {
		t.Fatal("an absent dropbox means the retrospective never ran")
	}
	if !strings.Contains(narration(t, log), "never ran") {
		t.Errorf("run.jsonl = %s, want the never-ran verdict", narration(t, log))
	}
	if !h.Worktree().Exists() {
		t.Error("a failed retrospective keeps the worktree as a breadcrumb")
	}
}

// BEH-568: a cap abort can fire after the skill wrote its up-front default `[]`.
// Left in place that reads as "ran, found nothing", masking the abort. The empty
// default is dropped so the "absent file = never ran" contract holds and the stage
// reports retry-after-reset.
func TestRetrospectiveCapAbortClearsTheDefaultEmptyDropbox(t *testing.T) {
	h := shipped(t, "PROJ-3")
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
	h := shipped(t, "PROJ-4")
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
	h, _ := handedOff(t, "PROJ-5")
	h.AgentFn = writesDropbox("[]")

	res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-5"), Args{Identifier: "PROJ-5"})

	if !res.OK {
		t.Fatalf("the retrospective itself succeeded, got %+v", res)
	}
	if !h.Worktree().Exists() {
		t.Error("an unpushed branch keeps its worktree")
	}
	if called(h, "pr-exists") {
		t.Error("an unpushed branch must never spend the gh round-trip")
	}
}

// BEH-539: the finding classes a prior run already filed are passed to the session
// as settled context — gathered BEFORE ClearDropbox wipes that prior dropbox.
func TestRetrospectivePassesAlreadyFiledFindingsAsSettledContext(t *testing.T) {
	h := shipped(t, "PROJ-6")
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
	h := shipped(t, "PROJ-7")

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

// #110: after a review that left CI to the PR loop, the retrospective runs over the
// open PR straight away and reaps the worktree. No CI result is read on the way —
// the PR loop checks the head branch out itself if the PR goes red.
func TestRetrospectiveAfterCIIsLeftToThePRLoopNeedsNoCIResult(t *testing.T) {
	h, _ := handedOff(t, "PROJ-4")
	h.CIFn = func(hostio.CIWatch) ci.Outcome {
		t.Error("no stage may wait on CI the PR loop owns")
		return ci.Outcome{}
	}
	retro := writesDropbox("[]")
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		if run.Label == retrospectiveSession {
			return retro(i, run)
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
	log := stageLog(t, "PROJ-4")
	args := Args{Identifier: "PROJ-4", LeaveCIToPRLoop: true}

	if res := Review(h, stageCfg(), log, args); !res.OK || res.Disposition != Shipped {
		t.Fatalf("review: %+v", res)
	}
	if res := Retrospective(h, stageCfg(), log, args); !res.OK {
		t.Fatalf("retrospective: %+v", res)
	}
	if h.Worktree().Exists() {
		t.Error("the worktree is reaped once the PR is open; the PR loop does not need it")
	}
}

// #185, replayed: a PR loop's conflict-resolution run left its logs under the
// loop's own dir — a rebasefix transcript, the prep and gate step logs, run.jsonl
// and a prior retrospective's transcript. The prompt must name that dir, relative
// to the checkout the session works in, and every file the run left there but the
// retrospective's own.
func TestRetrospectiveOverAPRLoopRunListsTheLoopsOwnTranscripts(t *testing.T) {
	h := shipped(t, "PR-178")
	cfg := stageCfg()
	cfg.ProjectPath = t.TempDir()
	logDir := filepath.Join(cfg.ProjectPath, ".agent-harness", "logs", "loops", "pr-fix", "PR-178")
	left := []string{
		"gate-build-20261101-200412.log",
		"gate-check-20261101-200412.log",
		"prep-20261101-200412.log",
		"rebasefix-20261101-200412.jsonl",
		"run.jsonl",
	}
	if err := os.MkdirAll(filepath.Join(logDir, "findings"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(left, "retrospective-20261101-190000.jsonl") {
		if err := os.WriteFile(filepath.Join(logDir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var prompt string
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		prompt = run.Prompt
		return writesDropbox("[]")(i, run)
	}
	cfg.Prompts.Retro = "/retrospective for {{.Identifier}}: read every transcript under `{{.LogDir}}/`."
	log, err := runlog.New(filepath.Dir(logDir), "PR-178")
	if err != nil {
		t.Fatal(err)
	}

	res := RetrospectiveOver(h, cfg, log, Subject{Ticket: ticket.Ticket{Identifier: "PR-178"}, Slug: "pr-178", LogDir: logDir}, false)

	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	// Absolute: the session starts in the worktree, not the checkout (#245).
	if want := "under `" + logDir + "/`"; !strings.Contains(prompt, want) {
		t.Errorf("prompt does not name the loop's log dir %q:\n%s", want, prompt)
	}
	for _, name := range left {
		if !strings.Contains(prompt, "`"+name+"`") {
			t.Errorf("prompt does not list %s", name)
		}
	}
	if strings.Contains(prompt, "retrospective-20261101-190000.jsonl") {
		t.Error("the retrospective's own stream is not one to mine")
	}
	if strings.Contains(prompt, "`findings`") {
		t.Error("the findings dir is not a transcript")
	}
}

// #220, replayed: a PR loop's retrospective whose body went out raw (it named a
// field this binary did not know) and never named the worktree. The session ran in
// the primary checkout, on an unrelated branch that also descended from the base,
// and the envelope's bare range showed it a different commit. The envelope now
// runs the range in the run's own worktree and names that worktree's branch.
func TestRetrospectiveRunsTheBranchRangeInTheRunsWorktree(t *testing.T) {
	h := shipped(t, "PR-178")
	cfg := stageCfg()
	cfg.ProjectPath = t.TempDir()
	wt := filepath.Join(cfg.ProjectPath, ".claude", "worktrees", "pr-178")
	logDir := filepath.Join(cfg.ProjectPath, ".agent-harness", "logs", "loops", "pr-fix", "PR-178")
	var prompt string
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		prompt = run.Prompt
		return writesDropbox("[]")(i, run)
	}
	cfg.Prompts.Retro = "/retrospective {{.Identifier}} {{.NotAField}}"
	log, err := runlog.New(filepath.Dir(logDir), "PR-178")
	if err != nil {
		t.Fatal(err)
	}

	res := RetrospectiveOver(h, cfg, log, Subject{
		Ticket: ticket.Ticket{Identifier: "PR-178"}, Slug: "pr-178", WorktreePath: wt, Branch: "fix/login-redirect", LogDir: logDir,
	}, false)

	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	for _, want := range []string{
		"`git -C " + wt + " log ",
		"`git -C " + wt + " diff ",
		"`" + wt + "` on branch `fix/login-redirect`",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
