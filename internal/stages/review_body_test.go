package stages

import (
	"errors"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// These tests reach the review stage's body — 541 lines with 14 exit paths, six
// inline closures and the BEH-624 fixed-point loop, none of which was reachable
// from a test before the hostio.Host seam. verify.Review and
// verify.ReviewVerdictRetry were always 100%-covered pure predicates; what was
// untested was the composition that fills them and acts on the verdict.
//
// Each test starts from handedOff — a real repo whose worktree carries one
// committed handoff — and drives the git half through it: a sibling's merge that
// makes the replay conflict or collapse, a session that leaves edits uncommitted
// or resolves the conflict for real.

func shellLabels(runs []hostio.ShellRun) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.Label
	}
	return out
}

// called reports whether the host saw exactly the call name.
func called(h *hostio.Fake, name string) bool {
	for _, c := range h.Calls {
		if c == name {
			return true
		}
	}
	return false
}

// assertNothingPushed fails when the branch reached origin or a push was even
// attempted.
func assertNothingPushed(t *testing.T, h *hostio.Fake, repo *gittest.Repo) {
	t.Helper()
	if called(h, "push") || called(h, "push-force") || repo.OnOrigin(h.Worktree().Branch()) != "" {
		t.Errorf("nothing may be pushed; calls = %v", h.Calls)
	}
}

// resolvesConflicts is a host whose review sessions reach their verdict and whose
// conflict-resolution session really resolves the conflict.
func resolvesConflicts(h *hostio.Fake, repo *gittest.Repo) {
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			repo.Resolve(h.Worktree())
			return session.Outcome{}
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
}

// The full green path: prep the worktree, run the cold review, re-run the gates
// host-side, rebase, force-push, open the PR, watch CI.
func TestReviewGreenPathPushesAndOpensThePR(t *testing.T) {
	h, repo := handedOff(t, "PROJ-1")
	repo.Merge("other.txt", "unrelated\n", "a sibling merged")
	log := stageLog(t, "PROJ-1")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-1"})

	if !res.OK || res.Disposition != Shipped {
		t.Fatalf("a green gate over a clean, reviewed worktree ships, got %+v", res)
	}
	if got := shellLabels(h.Shells); len(got) != 2 || got[0] != prepStep || got[1] != gateStep+"-check" {
		t.Errorf("shell runs = %v, want the prep container then one container per declared gate (BEH-634)", got)
	}
	for _, want := range []string{"push-force", "create-pr PROJ-1: a ticket", "watch-ci"} {
		if !called(h, want) {
			t.Errorf("calls = %v, want %q in the ship sequence", h.Calls, want)
		}
	}
	if called(h, "push") {
		t.Error("the pre-push must be force-with-lease — the unconditional rebase rewrote every SHA")
	}
	wt := h.Worktree()
	if tip, _ := wt.HeadSHA(); !wt.IsRebasedOnto(gitpkg.BaseRef) || repo.OnOrigin(wt.Branch()) != tip {
		t.Error("the branch must be rebased onto the latest main, and that tip pushed")
	}
}

// The gate's exit code is the ONLY thing that authorises a push. A red gate keeps
// the worktree, pushes nothing, and names the failing gate so it is diagnosable.
func TestReviewRedGateWithholdsThePushAndNamesTheGate(t *testing.T) {
	h, repo := handedOff(t, "PROJ-2")
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, gateStep) {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{}
	}
	log := stageLog(t, "PROJ-2")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-2"})

	if res.OK || res.Disposition == Shipped {
		t.Fatalf("a red gate ships nothing, got %+v", res)
	}
	assertNothingPushed(t, h, repo)
	if !strings.Contains(narration(t, log), `[gate \"check\"]`) {
		t.Errorf("the failing gate must be named (BEH-634); run.jsonl = %s", narration(t, log))
	}
}

// BEH-687: a docs-only diff feeds none of the gates, so the heavy host re-run
// validates nothing a prose edit could break — it is skipped and treated as green.
func TestReviewSkipsTheHostGateForADocsOnlyDiff(t *testing.T) {
	h, repo := fresh(t, "PROJ-3")
	wt := h.Worktree()
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	repo.Commit(wt, "docs/guide.md", "how to\n", "docs: explain the thing")
	cfg := stageCfg()
	cfg.DocsOnlyExcludedRoots = []string{"internal", "cmd"}

	res := Review(h, cfg, stageLog(t, "PROJ-3"), Args{Identifier: "PROJ-3"})

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
	h, _ := handedOff(t, "PROJ-4")
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
	h, _ := handedOff(t, "PROJ-5")
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
	h, _ := handedOff(t, "PROJ-6")
	h.AgentOutcome = session.Outcome{ReviewVerdictEmitted: true, ReviewBlocked: true}
	log := stageLog(t, "PROJ-6")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-6"})

	if res.OK || res.Disposition == Shipped {
		t.Fatalf("a blocked verdict ships nothing, got %+v", res)
	}
	if !strings.Contains(narration(t, log), "BEH-580") {
		t.Errorf("the blocked disposition must be narrated; run.jsonl = %s", narration(t, log))
	}
}

// BEH-603: a clean branch whose committed tip makes zero net change against
// origin/main — here because a sibling landed the same fix — is not a push and not
// a retryable failure: it is the recommend-close disposition, and the worktree is
// kept as the audit artifact.
func TestReviewEmptyDiffRecommendsClose(t *testing.T) {
	h, repo := handedOff(t, "PROJ-7")
	repo.Supersede("proj-7")
	log := stageLog(t, "PROJ-7")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-7"})

	if res.OK || res.Disposition != RecommendClose {
		t.Fatalf("a zero-net-diff branch recommends close, got %+v", res)
	}
	if !strings.Contains(narration(t, log), "BEH-603") {
		t.Errorf("the pre-rebase empty-diff check must be what caught it; run.jsonl = %s", narration(t, log))
	}
	assertNothingPushed(t, h, repo)
	if !h.Worktree().Exists() {
		t.Error("the worktree is kept as the audit artifact")
	}
}

// BEH-680: the rebase can collapse the branch to zero net change AFTER the
// pre-rebase check passed — a sibling PR landed the same fix and main moved on, so
// the pre-rebase diff is not empty but the replay drops the now-empty commit.
// Pushing then would hard-fail `gh pr create` with "No commits between …", so it
// routes to the same recommend-close disposition.
func TestReviewPostRebaseEmptyDiffRecommendsClose(t *testing.T) {
	h, repo := handedOff(t, "PROJ-8")
	repo.Supersede("proj-8")
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	log := stageLog(t, "PROJ-8")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-8"})

	if res.OK || res.Disposition != RecommendClose {
		t.Fatalf("a branch the rebase collapsed to nothing recommends close (BEH-680), got %+v", res)
	}
	if !strings.Contains(narration(t, log), "rebase collapsed the branch") {
		t.Errorf("the post-rebase collapse check must be what caught it; run.jsonl = %s", narration(t, log))
	}
	assertNothingPushed(t, h, repo)
}

// The precondition: without a worktree there is nothing to review, and the stage
// says which command to run first rather than burning a session.
func TestReviewWithoutAWorktreeIsACleanRefusal(t *testing.T) {
	h, _ := fresh(t, "PROJ-9")
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
	h, repo := handedOff(t, "PROJ-10")
	repo.Conflict("proj-10")
	resolvesConflicts(h, repo)

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
	if wt := h.Worktree(); !wt.IsRebasedOnto(gitpkg.BaseRef) || repo.OnOrigin(wt.Branch()) == "" {
		t.Error("the resolved branch must sit on origin/main and reach origin")
	}
}

// A conflict the session could not resolve keeps the worktree and leaves a tracker
// breadcrumb, so ~30 min of reviewed, gate-green work surfaces rather than sitting
// silent.
func TestReviewUnresolvedConflictKeepsTheWorktreeAndLeavesABreadcrumb(t *testing.T) {
	h, repo := handedOff(t, "PROJ-11")
	repo.Conflict("proj-11") // and the session resolves nothing

	res := Review(h, stageCfg(), stageLog(t, "PROJ-11"), Args{Identifier: "PROJ-11"})

	if res.OK {
		t.Fatalf("an unresolved conflict ships nothing, got %+v", res)
	}
	if len(h.Trk.Comments) != 1 || !strings.Contains(h.Trk.Comments[0], "BEH-581") {
		t.Errorf("comments = %v, want one breadcrumb naming the manual rebase", h.Trk.Comments)
	}
	assertNothingPushed(t, h, repo)
}

// BEH-597: a disjoint history is NOT a content conflict — the resolution session
// would burn a sandbox re-discovering an empty merge-base — so the stage leaves
// the branch as it was for manual recovery, and says so.
func TestReviewDisjointHistoryIsNotAContentConflict(t *testing.T) {
	h, repo := handedOff(t, "PROJ-12")
	repo.Disjoint(h.Worktree())
	before, _ := h.Worktree().HeadSHA()

	res := Review(h, stageCfg(), stageLog(t, "PROJ-12"), Args{Identifier: "PROJ-12"})

	if res.OK {
		t.Fatalf("a disjoint branch ships nothing, got %+v", res)
	}
	if got := labels(h.Agents); len(got) != 1 {
		t.Errorf("agent runs = %v, want no conflict-resolution session for a disjoint history (BEH-597)", got)
	}
	if after, _ := h.Worktree().HeadSHA(); after != before || !h.Worktree().Clean() {
		t.Errorf("the stopped replay must leave the branch clean on %s, got %s", before, after)
	}
}

// CI red after the auto-fix budget still leaves a reviewable PR for a human — NOT a
// ship failure, so the disposition stays Shipped and the loop's breaker must not
// count it (DESIGN.md §Circuit breaker).
func TestReviewRedCIStillReportsShipped(t *testing.T) {
	h, _ := handedOff(t, "PROJ-13")
	h.CI = ci.Outcome{OK: false, Reason: "checks still red after 3 fix attempts"}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-13"), Args{Identifier: "PROJ-13"})

	if res.OK {
		t.Fatal("red CI is not a success")
	}
	if res.Disposition != Shipped {
		t.Error("the PR exists, so the breaker must see a ship (DESIGN.md §Circuit breaker)")
	}
}

// #110: a ticket loop that leaves CI to the PR loop does not wait on CI. The stage
// ships the moment the PR is open and never watches, so the retrospective, the
// worktree reap and the next ticket start at once; a check that goes red later is
// the PR loop's CIRed candidate.
func TestReviewLeavingCIToThePRLoopShipsOnceThePRIsOpen(t *testing.T) {
	h, _ := handedOff(t, "PROJ-16")
	h.CIFn = func(hostio.CIWatch) ci.Outcome {
		t.Error("the review stage must not watch CI it leaves to the PR loop")
		return ci.Outcome{OK: false, Reason: "red"}
	}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-16"), Args{Identifier: "PROJ-16", LeaveCIToPRLoop: true})

	if !res.OK || res.Disposition != Shipped {
		t.Fatalf("an open PR is the stage's job done, got %+v", res)
	}
	opened := false
	for _, c := range h.Calls {
		opened = opened || strings.HasPrefix(c, "create-pr ")
	}
	if !opened || called(h, "watch-ci") {
		t.Errorf("calls = %v, want the PR opened and no CI watch", h.Calls)
	}
}

// BEH-602: the branch became a no-op against the latest origin/main only after the
// pre-push rebase, so the PR is open with nothing for CI to validate. Honour the
// recommend-close disposition rather than burn the poll budget — and report it as the
// run's disposition even though a PR was opened, since that is what makes the loop
// close the ticket (BEH-682) instead of reading a no-op PR as a clean ship.
func TestReviewCIRecommendCloseKeepsThePRAndTheWorktree(t *testing.T) {
	h, _ := handedOff(t, "PROJ-14")
	h.CI = ci.Outcome{RecommendClose: true, Reason: "branch is a no-op against origin/main"}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-14"), Args{Identifier: "PROJ-14"})

	if res.Disposition != RecommendClose || res.OK {
		t.Fatalf("a post-PR no-op recommends close with the PR kept, got %+v", res)
	}
}

// BEH-571: an auto-fix session that hit an active spending cap is a billing window
// that resets, not an unfixable CI. The PR is already open, so the run's disposition
// is the ship it is — a shipped PR outranks a cap abort for both the loop (which
// never releases a shipped ticket) and the breaker (which counts it a success).
func TestReviewCISpendingCapKeepsTheShippedPR(t *testing.T) {
	h, _ := handedOff(t, "PROJ-15")
	h.CI = ci.Outcome{SpendingCapAbort: true}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-15"), Args{Identifier: "PROJ-15"})

	if res.Disposition != Shipped || res.OK {
		t.Fatalf("a capped auto-fix keeps the shipped PR, got %+v", res)
	}
}

// BEH-559: a review killed mid-edit leaves its in-progress fixes uncommitted. They
// are checkpoint-committed AFTER the push decision (made on the pre-checkpoint
// cleanliness), so an unverified half-fix is preserved but never pushed.
func TestReviewCheckpointsUncommittedEditsWithoutPushingThem(t *testing.T) {
	h, repo := handedOff(t, "PROJ-16")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		repo.Dirty(h.Worktree())
		return session.Outcome{ExitCode: sandbox.ExitOOMKill}
	}

	res := Review(h, stageCfg(), stageLog(t, "PROJ-16"), Args{Identifier: "PROJ-16"})

	if res.OK {
		t.Fatal("a dirty worktree is never pushed")
	}
	if want := strings.SplitN(gitpkg.CheckpointMessage("PROJ-16", reviewSession), "\n", 2)[0]; repo.Subject(h.Worktree()) != want {
		t.Errorf("in-progress edits must be checkpoint-committed (BEH-559); HEAD is %q", repo.Subject(h.Worktree()))
	}
	assertNothingPushed(t, h, repo)
}

// BEH-641: a Consumer that declares no post_create has nothing to re-provision, so
// the prep container is skipped entirely rather than run with an empty command.
func TestReviewSkipsPrepWhenTheConsumerDeclaresNoPostCreate(t *testing.T) {
	h, _ := handedOff(t, "PROJ-17")
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
	h, repo := handedOff(t, "PROJ-18")
	h.CreatePRErr = errors.New("gh: API rate limit exceeded")
	log := stageLog(t, "PROJ-18")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-18"})

	if res.OK || res.Disposition == Shipped {
		t.Fatalf("no PR means no ship, got %+v", res)
	}
	if repo.OnOrigin(h.Worktree().Branch()) == "" {
		t.Error("the branch reached origin before gh failed")
	}
	if !strings.Contains(narration(t, log), "open the PR manually") {
		t.Errorf("run.jsonl = %s, want the manual-recovery hint", narration(t, log))
	}
}

// The gate list is the Consumer's, run in order, one container each (BEH-634).
func TestReviewRunsOneContainerPerDeclaredGateInOrder(t *testing.T) {
	h, _ := handedOff(t, "PROJ-19")
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
// fix.Env.CI is what the review hands the CI watch: a sandboxed diagnose-and-fix
// session, followed by ground-truth enforcement. The watch itself is host-side
// (ADR-0002), so the Fake drives the callback the way the real GhDriver would.

// The clean case: the fix session exits 0 over a clean worktree having found no
// code defect, and the harness adds a re-trigger commit so CI re-runs against a
// fresh HEAD (BEH-561).
func TestReviewCIFixAcceptsACommittedFix(t *testing.T) {
	h, repo := handedOff(t, "PROJ-20")
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
	if want := strings.SplitN(gitpkg.CIRerunMessage(), "\n", 2)[0]; repo.Subject(h.Worktree()) != want {
		t.Errorf("HEAD is %q, want the re-trigger commit so CI re-runs against a fresh HEAD (BEH-561)", repo.Subject(h.Worktree()))
	}
}

// Ground truth, never the agent's say-so: a fix session that left the worktree
// dirty has nothing trustworthy to push.
func TestReviewCIFixRejectsAnUncommittedFix(t *testing.T) {
	h, repo := handedOff(t, "PROJ-21")
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if strings.HasPrefix(run.Label, "cifix") {
			repo.Dirty(h.Worktree()) // the agent edited but never committed
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
	if repo.Subject(h.Worktree()) != gittest.HandoffSubject {
		t.Errorf("no re-trigger commit may follow a refused fix; HEAD is %q", repo.Subject(h.Worktree()))
	}
}

// BEH-571: a fix session aborted by a spending cap is its own retry-after-reset
// class, so the watch defers rather than counting a 1-second no-op as a failure.
func TestReviewCIFixSurfacesASpendingCapAsItsOwnClass(t *testing.T) {
	h, _ := handedOff(t, "PROJ-22")
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

// #46: review prep the shell could not run aborts the stage before the session
// launches, the same way post_create does in implementation.
func TestReviewAbortsWhenPrepCannotRun(t *testing.T) {
	for _, code := range []int{127, 126} {
		h, _ := handedOff(t, "PROJ-46")
		h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
			if s.Label == prepStep {
				return session.Outcome{ExitCode: code, StderrTail: "bash: line 1: pnpm: command not found"}
			}
			return session.Outcome{}
		}
		log := stageLog(t, "PROJ-46")

		res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-46"})

		if res.OK || res.Disposition == Shipped {
			t.Errorf("exit %d: an unrunnable prep ships nothing, got %+v", code, res)
		}
		if len(h.Agents) != 0 {
			t.Errorf("exit %d: no review session may launch behind an unrunnable prep, got %v", code, h.Agents)
		}
		if got := shellLabels(h.Shells); len(got) != 1 {
			t.Errorf("exit %d: shell runs = %v, want only the prep — no gate behind it", code, got)
		}
		said := narration(t, log)
		for _, want := range []string{"`pnpm`", "not on PATH in the worktree-command container"} {
			if !strings.Contains(said, want) {
				t.Errorf("exit %d: run.jsonl should carry %q; got %s", code, want, said)
			}
		}
	}
}

// #46: a host gate that cannot find its tool is reported as the environment, with
// the gate's stderr tail in the run log, not as a red gate that reads as a code
// regression.
func TestReviewUnrunnableGateIsReportedAsTheEnvironment(t *testing.T) {
	h, repo := handedOff(t, "PROJ-48")
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, gateStep) {
			return session.Outcome{ExitCode: 2, StderrTail: "make: golangci-lint: No such file or directory\nmake: *** [Makefile:12: lint] Error 127"}
		}
		return session.Outcome{}
	}
	log := stageLog(t, "PROJ-48")

	res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-48"})

	if res.OK {
		t.Fatalf("nothing may ship behind an unrunnable gate, got %+v", res)
	}
	assertNothingPushed(t, h, repo)
	said := narration(t, log)
	if strings.Contains(said, "gate re-run is red") {
		t.Errorf("an unrunnable gate must not read as a red gate; run.jsonl = %s", said)
	}
	for _, want := range []string{"environment, not code", "make: golangci-lint: No such file or directory"} {
		if !strings.Contains(said, want) {
			t.Errorf("run.jsonl should carry %q; got %s", want, said)
		}
	}
}

// clearVerdict is a review that reached a clear Disposition, the only verdict a
// re-run is told.
var clearVerdict = session.Outcome{ReviewVerdictEmitted: true, ReviewDisposition: "Disposition: clear — nothing to fix"}

// #44: a re-run whose implementation added no commits hands the review the verdict
// the last review emitted and the range it covered, so the reviewer re-emits it
// instead of re-deriving it and stacking a churn commit. The first review is cold.
func TestReviewReRunOverAnUnchangedBranchIsToldThePriorVerdict(t *testing.T) {
	h, _ := handedOff(t, "PROJ-40")
	h.AgentOutcome = clearVerdict
	log := stageLog(t, "PROJ-40")

	Review(h, stageCfg(), log, Args{Identifier: "PROJ-40"})
	head, _ := h.Worktree().HeadSHA()
	Review(h, stageCfg(), log, Args{Identifier: "PROJ-40"})

	if len(h.Agents) != 2 {
		t.Fatalf("agent runs = %v, want one review session per run", labels(h.Agents))
	}
	if strings.Contains(h.Agents[0].Prompt, "UNCHANGED SINCE THE LAST REVIEW") {
		t.Error("the first review has no prior verdict and must review cold")
	}
	for _, want := range []string{"origin/main.." + head, "Disposition: clear — nothing to fix"} {
		if !strings.Contains(h.Agents[1].Prompt, want) {
			t.Errorf("the re-run's review prompt is missing %q", want)
		}
	}
}

// A branch that moved since the last review — the implementation re-run added a
// commit — is reviewed cold: the prior verdict covered different commits.
func TestReviewReRunOverAMovedBranchReviewsCold(t *testing.T) {
	h, repo := handedOff(t, "PROJ-41")
	h.AgentOutcome = clearVerdict
	log := stageLog(t, "PROJ-41")

	Review(h, stageCfg(), log, Args{Identifier: "PROJ-41"})
	repo.Commit(h.Worktree(), "more.txt", "more\n", "feat: a further commit")
	Review(h, stageCfg(), log, Args{Identifier: "PROJ-41"})

	if strings.Contains(h.Agents[1].Prompt, "UNCHANGED SINCE THE LAST REVIEW") {
		t.Error("a branch with new commits since the last review must be reviewed cold")
	}
}

// Landing rebases the branch onto a main that moved, rewriting every SHA. The
// verdict still covers the landed commits, so a re-run over the landed tip is told
// it.
func TestReviewRecordsTheLandedHeadAsReviewed(t *testing.T) {
	h, repo := handedOff(t, "PROJ-42")
	h.AgentOutcome = clearVerdict
	reviewed, _ := h.Worktree().HeadSHA()
	repo.Merge("other.txt", "unrelated\n", "a sibling merged")
	log := stageLog(t, "PROJ-42")

	Review(h, stageCfg(), log, Args{Identifier: "PROJ-42"})
	landed, _ := h.Worktree().HeadSHA()
	Review(h, stageCfg(), log, Args{Identifier: "PROJ-42"})

	if landed == reviewed {
		t.Fatal("the landing should have replayed the branch onto the moved main")
	}
	if !strings.Contains(h.Agents[1].Prompt, "origin/main.."+landed) {
		t.Errorf("the re-run over the landed tip must be told the prior verdict; prompt:\n%s", h.Agents[1].Prompt)
	}
}

// A review that never reached a Disposition left no verdict to carry forward.
func TestReviewWithoutADispositionRecordsNothing(t *testing.T) {
	h, _ := handedOff(t, "PROJ-43")
	log := stageLog(t, "PROJ-43")

	Review(h, stageCfg(), log, Args{Identifier: "PROJ-43"})
	Review(h, stageCfg(), log, Args{Identifier: "PROJ-43"})

	if strings.Contains(h.Agents[1].Prompt, "UNCHANGED SINCE THE LAST REVIEW") {
		t.Error("no Disposition was emitted, so there is no prior verdict to hand on")
	}
}

// A blocked verdict waits on a human answer that lands on the ticket, not the
// branch: a re-run over the same HEAD reviews cold rather than re-emit the block.
func TestReviewReRunAfterABlockedVerdictReviewsCold(t *testing.T) {
	h, _ := handedOff(t, "PROJ-44")
	h.AgentOutcome = session.Outcome{ReviewVerdictEmitted: true, ReviewBlocked: true, ReviewDisposition: "Disposition: blocked — needs a human call"}
	log := stageLog(t, "PROJ-44")

	Review(h, stageCfg(), log, Args{Identifier: "PROJ-44"})
	Review(h, stageCfg(), log, Args{Identifier: "PROJ-44"})

	if strings.Contains(h.Agents[1].Prompt, "UNCHANGED SINCE THE LAST REVIEW") {
		t.Error("a blocked verdict must not be carried forward to a re-run")
	}
}
