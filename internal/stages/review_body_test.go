package stages

import (
	"errors"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// These tests reach the review stage's body — 541 lines with 14 exit paths, six
// inline closures and the BEH-624 fixed-point loop, none of which was reachable
// from a test before the hostio.Host seam. verify.Review and
// verify.ReviewVerdictRetry were always 100%-covered pure predicates; what was
// untested was the composition that fills them and acts on the verdict.

func shellLabels(runs []hostio.ShellRun) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.Label
	}
	return out
}

// The full green path: prep the worktree, run the cold review, re-run the gates
// host-side, rebase, force-push, open the PR, watch CI.
func TestReviewGreenPathPushesAndOpensThePR(t *testing.T) {
	h := hostio.NewFake()
	log := stageLog(t, "PROJ-1")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-1"})

	if !res.OK || !res.ReachedPushedPR {
		t.Fatalf("a green gate over a clean, reviewed worktree ships, got %+v", res)
	}
	if got := shellLabels(h.Shells); len(got) != 2 || got[0] != prepStep || got[1] != gateStep+"-check" {
		t.Errorf("shell runs = %v, want the prep container then one container per declared gate (BEH-634)", got)
	}
	calls := strings.Join(h.Calls, "|")
	for _, want := range []string{"rebase proj-1", "push-force proj-1", "create-pr", "watch-ci proj-1"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls = %v, want %q in the ship sequence", h.Calls, want)
		}
	}
	if strings.Contains(calls, "|push proj-1|") {
		t.Error("the pre-push must be force-with-lease — the unconditional rebase rewrote every SHA")
	}
}

// The gate's exit code is the ONLY thing that authorises a push. A red gate keeps
// the worktree, pushes nothing, and names the failing gate so it is diagnosable.
func TestReviewRedGateWithholdsThePushAndNamesTheGate(t *testing.T) {
	h := hostio.NewFake()
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, gateStep) {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{}
	}
	log := stageLog(t, "PROJ-2")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-2"})

	if res.OK || res.ReachedPushedPR {
		t.Fatalf("a red gate ships nothing, got %+v", res)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "push") {
		t.Errorf("nothing may be pushed behind a red gate; calls = %v", h.Calls)
	}
	if !strings.Contains(narration(t, log), `[gate \"check\"]`) {
		t.Errorf("the failing gate must be named (BEH-634); run.jsonl = %s", narration(t, log))
	}
}

// BEH-687: a docs-only diff feeds none of the gates, so the heavy host re-run
// validates nothing a prose edit could break — it is skipped and treated as green.
func TestReviewSkipsTheHostGateForADocsOnlyDiff(t *testing.T) {
	h := hostio.NewFake()
	h.DocsOnly = true

	res := Review(h, stageCfg(), stageLog(t, "PROJ-3"), Args{Identifier: "PROJ-3"})

	if !res.OK {
		t.Fatalf("a docs-only diff still ships, got %+v", res)
	}
	for _, label := range shellLabels(h.Shells) {
		if strings.HasPrefix(label, gateStep) {
			t.Errorf("no gate container may run for a docs-only diff (BEH-687), got %v", shellLabels(h.Shells))
		}
	}
}

// BEH-624: a review that exits cleanly one turn short of its verdict, over a clean
// worktree the host gate already proved green, is re-launched in-stage — and the
// gate is re-run after it, because the re-launch may have committed fixes. This is
// the fixed-point loop whose condition re-evaluates the (tested) predicate against
// state the body just mutated; the predicate was covered, the loop was not.
func TestReviewRelaunchesASessionThatStoppedShortOfItsVerdict(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = func(call int, _ hostio.AgentRun) session.Outcome {
		// The first session stops short; the re-launch reaches its verdict.
		return session.Outcome{ReviewVerdictEmitted: call > 1}
	}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-4"), Args{Identifier: "PROJ-4"})

	if !res.OK {
		t.Fatalf("the re-launch reached its verdict over a verified diff, so it ships; got %+v", res)
	}
	want := []string{reviewSession, reviewSession + "-retry2"}
	if got := labels(h.Agents); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("agent runs = %v, want the review re-launched once under a distinct label %v (BEH-624)", got, want)
	}
	if got := shellLabels(h.Shells); len(got) != 3 || got[2] != gateStep+"-retry2-check" {
		t.Errorf("shell runs = %v, want the gate re-run after the re-launch under its own label", got)
	}
}

// The re-launch is bounded: a session that never reaches its verdict falls through
// to fail-closed rather than spinning.
func TestReviewVerdictRelaunchIsBounded(t *testing.T) {
	h := hostio.NewFake()
	h.AgentOutcome = session.Outcome{} // never emits a verdict

	res := Review(h, stageCfg(), stageLog(t, "PROJ-5"), Args{Identifier: "PROJ-5"})

	if res.OK {
		t.Fatal("an unreviewed diff must fail the push closed (BEH-569)")
	}
	if got := labels(h.Agents); len(got) != reviewVerdictMaxAttempts {
		t.Errorf("agent runs = %v, want exactly %d (BEH-624 bound)", got, reviewVerdictMaxAttempts)
	}
}

// BEH-580: a verdict that declared a blocked disposition found a Blocker the
// autonomous reviewer could not resolve. There is no human to approve it, so the
// push fails closed rather than shipping the finding to a PR unaddressed.
func TestReviewBlockedVerdictFailsThePushClosed(t *testing.T) {
	h := hostio.NewFake()
	h.AgentOutcome = session.Outcome{ReviewVerdictEmitted: true, ReviewBlocked: true}
	log := stageLog(t, "PROJ-6")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-6"})

	if res.OK || res.ReachedPushedPR {
		t.Fatalf("a blocked verdict ships nothing, got %+v", res)
	}
	if !strings.Contains(narration(t, log), "BEH-580") {
		t.Errorf("the blocked disposition must be narrated; run.jsonl = %s", narration(t, log))
	}
}

// BEH-603: a clean branch whose committed tip makes zero net change against
// origin/main is not a push and not a retryable failure — it is the recommend-close
// disposition, and the worktree is kept as the audit artifact.
func TestReviewEmptyDiffRecommendsClose(t *testing.T) {
	h := hostio.NewFake()
	h.DiffEmpty = true

	res := Review(h, stageCfg(), stageLog(t, "PROJ-7"), Args{Identifier: "PROJ-7"})

	if res.OK || !res.RecommendClose {
		t.Fatalf("a zero-net-diff branch recommends close, got %+v", res)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "push") {
		t.Errorf("nothing may be pushed for a zero-net-diff branch; calls = %v", h.Calls)
	}
}

// BEH-680: the rebase can collapse the branch to zero net change AFTER the
// pre-rebase check passed (a sibling PR landed the same fix during the multi-minute
// gate). Pushing then would hard-fail `gh pr create` with "No commits between …",
// so it routes to the same recommend-close disposition.
func TestReviewPostRebaseEmptyDiffRecommendsClose(t *testing.T) {
	h := hostio.NewFake()
	h.DiffEmpty = false
	res := Review(collapsing{h}, stageCfg(), stageLog(t, "PROJ-8"), Args{Identifier: "PROJ-8"})

	if res.OK || !res.RecommendClose {
		t.Fatalf("a branch the rebase collapsed to nothing recommends close (BEH-680), got %+v", res)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "push-force") {
		t.Errorf("nothing may be pushed once the rebase collapsed the branch; calls = %v", h.Calls)
	}
}

// collapsing is a Fake whose branch becomes a no-op against origin/main the moment
// it is rebased — the BEH-680 race, where a sibling PR landed the same fix during
// the multi-minute gate.
type collapsing struct{ *hostio.Fake }

func (c collapsing) Rebase(slug string) hostio.RebaseResult {
	v := c.Fake.Rebase(slug)
	c.Fake.DiffEmpty = true
	return v
}

// The precondition: without a worktree there is nothing to review, and the stage
// says which command to run first rather than burning a session.
func TestReviewWithoutAWorktreeIsACleanRefusal(t *testing.T) {
	h := hostio.NewFake()
	h.Exists = false
	log := stageLog(t, "PROJ-9")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-9"})

	if res.OK || res.Err != nil {
		t.Fatalf("a missing worktree is a plain not-OK, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("no session may launch without a worktree, got %v", labels(h.Agents))
	}
	if !strings.Contains(narration(t, log), "run `implementation PROJ-9` first") {
		t.Errorf("the refusal must name the command to run first; run.jsonl = %s", narration(t, log))
	}
}

// BEH-581: a genuine pre-push content conflict no longer dead-ends. A bounded
// sandboxed conflict-resolution session runs over the worktree, the host gate is
// re-run over the rewritten tree, and only then does the branch push.
func TestReviewResolvesAPrePushConflictThenRegatesAndPushes(t *testing.T) {
	h := hostio.NewFake()
	h.RebaseVerdict = hostio.RebaseConflict
	h.Rebased = true

	res := Review(h, stageCfg(), stageLog(t, "PROJ-10"), Args{Identifier: "PROJ-10"})

	if !res.OK {
		t.Fatalf("a cleanly resolved, re-gated conflict ships, got %+v", res)
	}
	if got := labels(h.Agents); len(got) != 2 || got[1] != "rebasefix" {
		t.Fatalf("agent runs = %v, want a conflict-resolution session after the review (BEH-581)", got)
	}
	if got := shellLabels(h.Shells); len(got) != 3 || got[2] != gateStep+"-postrebase-check" {
		t.Errorf("shell runs = %v, want the gate re-run over the resolved tree before the push", got)
	}
}

// A conflict the session could not resolve keeps the worktree and leaves a tracker
// breadcrumb, so ~30 min of reviewed, gate-green work surfaces rather than sitting
// silent.
func TestReviewUnresolvedConflictKeepsTheWorktreeAndLeavesABreadcrumb(t *testing.T) {
	h := hostio.NewFake()
	h.RebaseVerdict = hostio.RebaseConflict
	h.Rebased = false // the session never actually rebased

	res := Review(h, stageCfg(), stageLog(t, "PROJ-11"), Args{Identifier: "PROJ-11"})

	if res.OK {
		t.Fatalf("an unresolved conflict ships nothing, got %+v", res)
	}
	if len(h.Trk.Comments) != 1 || !strings.Contains(h.Trk.Comments[0], "BEH-581") {
		t.Errorf("comments = %v, want one breadcrumb naming the manual rebase", h.Trk.Comments)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "push-force") {
		t.Errorf("nothing may be pushed after an unresolved conflict; calls = %v", h.Calls)
	}
}

// BEH-597: a disjoint history is NOT a content conflict — the resolution session
// would burn a sandbox re-discovering an empty merge-base — so the stage aborts the
// rebase, keeps the worktree for manual recovery, and says so.
func TestReviewDisjointHistoryIsNotAContentConflict(t *testing.T) {
	h := hostio.NewFake()
	h.RebaseVerdict = hostio.RebaseConflict
	h.Disjoint = true

	res := Review(h, stageCfg(), stageLog(t, "PROJ-12"), Args{Identifier: "PROJ-12"})

	if res.OK {
		t.Fatalf("a disjoint branch ships nothing, got %+v", res)
	}
	if got := labels(h.Agents); len(got) != 1 {
		t.Errorf("agent runs = %v, want no conflict-resolution session for a disjoint history (BEH-597)", got)
	}
	if !strings.Contains(strings.Join(h.Calls, "|"), "abort-rebase") {
		t.Errorf("the in-progress rebase must be aborted; calls = %v", h.Calls)
	}
}

// CI red after the auto-fix budget still leaves a reviewable PR for a human — NOT a
// ship failure, so ReachedPushedPR stays true and the loop's breaker must not count
// it (DESIGN.md §Circuit breaker).
func TestReviewRedCIStillReportsReachedPushedPR(t *testing.T) {
	h := hostio.NewFake()
	h.CI = ci.Outcome{OK: false, Reason: "checks still red after 3 fix attempts"}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-13"), Args{Identifier: "PROJ-13"})

	if res.OK {
		t.Fatal("red CI is not a success")
	}
	if !res.ReachedPushedPR {
		t.Error("the PR exists, so the breaker must see a ship (DESIGN.md §Circuit breaker)")
	}
}

// BEH-602: the branch became a no-op against the latest origin/main only after the
// pre-push rebase, so the PR is open with nothing for CI to validate. Honour the
// recommend-close disposition rather than burn the poll budget.
func TestReviewCIRecommendCloseKeepsThePRAndTheWorktree(t *testing.T) {
	h := hostio.NewFake()
	h.CI = ci.Outcome{RecommendClose: true, Reason: "branch is a no-op against origin/main"}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-14"), Args{Identifier: "PROJ-14"})

	if !res.RecommendClose || !res.ReachedPushedPR || res.OK {
		t.Fatalf("a post-PR no-op recommends close with the PR kept, got %+v", res)
	}
}

// BEH-571: an auto-fix session that hit an active spending cap is a billing window
// that resets, not an unfixable CI — the loop must read retry-after-reset.
func TestReviewCISpendingCapIsRetryAfterReset(t *testing.T) {
	h := hostio.NewFake()
	h.CI = ci.Outcome{SpendingCapAbort: true}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-15"), Args{Identifier: "PROJ-15"})

	if !res.SpendingCapAbort || !res.ReachedPushedPR || res.OK {
		t.Fatalf("a capped auto-fix defers with the PR kept, got %+v", res)
	}
}

// BEH-559: a review killed mid-edit leaves its in-progress fixes uncommitted. They
// are checkpoint-committed AFTER the push decision (made on the pre-checkpoint
// cleanliness), so an unverified half-fix is preserved but never pushed.
func TestReviewCheckpointsUncommittedEditsWithoutPushingThem(t *testing.T) {
	h := hostio.NewFake()
	h.Clean = false
	h.AgentOutcome = session.Outcome{ExitCode: sandbox.ExitOOMKill}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-16"), Args{Identifier: "PROJ-16"})

	if res.OK {
		t.Fatal("a dirty worktree is never pushed")
	}
	if !strings.Contains(strings.Join(h.Calls, "|"), "checkpoint PROJ-16 review") {
		t.Errorf("in-progress edits must be checkpoint-committed (BEH-559); calls = %v", h.Calls)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "push") {
		t.Errorf("a checkpoint is preserved, never pushed; calls = %v", h.Calls)
	}
}

// BEH-641: a Consumer that declares no post_create has nothing to re-provision, so
// the prep container is skipped entirely rather than run with an empty command.
func TestReviewSkipsPrepWhenTheConsumerDeclaresNoPostCreate(t *testing.T) {
	h := hostio.NewFake()
	cfg := stageCfg()
	cfg.PostCreate = ""

	if res := Review(h, cfg, stageLog(t, "PROJ-17"), Args{Identifier: "PROJ-17"}); !res.OK {
		t.Fatalf("a Consumer with no post_create still ships, got %+v", res)
	}
	for _, label := range shellLabels(h.Shells) {
		if label == prepStep {
			t.Errorf("no prep container may run when no post_create is declared (BEH-641), got %v", shellLabels(h.Shells))
		}
	}
}

// A failed `gh pr create` leaves a pushed branch with no PR — not a ship, and the
// operator is told to open it manually.
func TestReviewPRCreationFailureIsNotAShip(t *testing.T) {
	h := hostio.NewFake()
	h.CreatePRErr = errors.New("gh: API rate limit exceeded")
	log := stageLog(t, "PROJ-18")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-18"})

	if res.OK || res.ReachedPushedPR {
		t.Fatalf("no PR means no ship, got %+v", res)
	}
	if !strings.Contains(narration(t, log), "open the PR manually") {
		t.Errorf("run.jsonl = %s, want the manual-recovery hint", narration(t, log))
	}
}

// The gate list is the Consumer's, run in order, one container each (BEH-634).
func TestReviewRunsOneContainerPerDeclaredGateInOrder(t *testing.T) {
	h := hostio.NewFake()
	cfg := stageCfg()
	cfg.Gates = []config.Gate{
		{Name: "lint", Command: "golangci-lint run"},
		{Name: "test", Command: "go test ./..."},
	}

	Review(h, cfg, stageLog(t, "PROJ-19"), Args{Identifier: "PROJ-19"})

	want := []string{prepStep, gateStep + "-lint", gateStep + "-test"}
	if got := shellLabels(h.Shells); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("shell runs = %v, want %v", got, want)
	}
	if h.Shells[1].Command != "golangci-lint run" || h.Shells[2].Command != "go test ./..." {
		t.Errorf("each gate must run its declared command verbatim, got %q / %q", h.Shells[1].Command, h.Shells[2].Command)
	}
}

// --- the CI auto-fix callback -----------------------------------------------
// ciFixRunner is what the host hands the CI watch: a sandboxed diagnose-and-fix
// session, followed by ground-truth enforcement. The watch itself is host-side
// (ADR-0002), so the Fake drives the callback the way the real GhDriver would.

// The clean case: the fix session exits 0 over a committed (clean) worktree, and
// the harness adds a re-trigger commit so CI re-runs against a fresh HEAD (BEH-561).
func TestReviewCIFixAcceptsACommittedFix(t *testing.T) {
	h := hostio.NewFake()
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: true, Reason: "green after the fix"}
	}

	if res := Review(h, stageCfg(), stageLog(t, "PROJ-20"), Args{Identifier: "PROJ-20"}); !res.OK {
		t.Fatalf("a fixed, green CI ships, got %+v", res)
	}
	if fixErr != nil {
		t.Fatalf("a committed fix over a clean worktree is accepted, got %v", fixErr)
	}
	if got := labels(h.Agents); len(got) != 2 || got[1] != "cifix-1" {
		t.Errorf("agent runs = %v, want one auto-fix session labelled per attempt", got)
	}
	if !strings.Contains(strings.Join(h.Calls, "|"), "ci-rerun-commit") {
		t.Errorf("calls = %v, want the re-trigger commit so CI re-runs against a fresh HEAD (BEH-561)", h.Calls)
	}
}

// Ground truth, never the agent's say-so: a fix session that left the worktree
// dirty has nothing trustworthy to push.
func TestReviewCIFixRejectsAnUncommittedFix(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if strings.HasPrefix(run.Label, "cifix") {
			h.Clean = false // the agent edited but never committed
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: false, Reason: "still red"}
	}

	Review(h, stageCfg(), stageLog(t, "PROJ-21"), Args{Identifier: "PROJ-21"})

	if fixErr == nil || !strings.Contains(fixErr.Error(), "left uncommitted changes") {
		t.Fatalf("an uncommitted fix must be refused, got %v", fixErr)
	}
	if strings.Contains(strings.Join(h.Calls, "|"), "ci-rerun-commit") {
		t.Errorf("no re-trigger commit may follow a refused fix; calls = %v", h.Calls)
	}
}

// BEH-571: a fix session aborted by a spending cap is its own retry-after-reset
// class, so the watch defers rather than counting a 1-second no-op as a failure.
func TestReviewCIFixSurfacesASpendingCapAsItsOwnClass(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if strings.HasPrefix(run.Label, "cifix") {
			return session.Outcome{ExitCode: 1, SpendingCapAbort: true}
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("", false)
		return ci.Outcome{SpendingCapAbort: true}
	}

	Review(h, stageCfg(), stageLog(t, "PROJ-22"), Args{Identifier: "PROJ-22"})

	if !errors.Is(fixErr, ci.ErrSpendingCapActive) {
		t.Fatalf("a capped fix session is the retry-after-reset class (BEH-571), got %v", fixErr)
	}
}
