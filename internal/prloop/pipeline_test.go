package prloop

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
)

func pipelineCfg() config.Config {
	return config.Config{
		Host: config.Host{ProjectPath: "/fake/checkout", ReviewTimeout: 30 * time.Minute, RetrospectiveTimeout: 20 * time.Minute},
		Project: config.Project{
			BranchPrefix: "feat",
			PostCreate:   "pnpm install --frozen-lockfile",
			Gates:        []config.Gate{{Name: "check", Command: "pnpm check"}},
		},
	}
}

// prDoctor is the built-in PR loop, whose steps are the hard-coded PR pipeline.
func prDoctor() config.Loop {
	l, _ := config.Project{}.LoopNamed(config.PRDoctorLoopName)
	return l
}

// withoutSteps is the built-in PR loop with some of its optional steps left out.
func withoutSteps(drop ...string) config.Loop {
	l := prDoctor()
	var kept []config.Step
	for _, s := range l.Steps {
		if !strings.Contains(strings.Join(drop, ","), s.Use) {
			kept = append(kept, s)
		}
	}
	l.Steps = kept
	return l
}

func pipelineLog(t *testing.T) *runlog.Logger {
	t.Helper()
	log, err := runlog.New(t.TempDir(), "PR-12")
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return log
}

// prHost is a healthy Fake for it's PR over a real repo: the PR's head branch is on
// origin, main has moved on past where it was cut, and it is polled at the head
// origin really has. A conflict-resolution session really brings the branch onto
// origin/main; every other session does what NewFake scripts.
func prHost(t *testing.T, it Item) (*hostio.Fake, Item, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			repo.Resolve(h.Worktree())
		}
		return h.AgentOutcome
	}
	return h, it, repo
}

func harnessItem(k Kind) Item {
	return Item{PR: github.PullRequest{Number: 12, Title: "#7: a ticket", HeadBranch: "feat/gh-7", BaseBranch: "main"}, Kind: k, Harness: true}
}

func humanItem(k Kind) Item {
	return Item{PR: github.PullRequest{Number: 12, Title: "Widget", HeadBranch: "alice/widget", BaseBranch: "main"}, Kind: k}
}

func agentLabels(h *hostio.Fake) []string {
	var out []string
	for _, a := range h.Agents {
		out = append(out, a.Label)
	}
	return out
}

func calls(h *hostio.Fake) string { return strings.Join(h.Calls, "|") + "|" }

// A conflict on a harness branch: check the branch out, provision it, resolve by
// rebasing, re-gate, push force-with-lease, watch CI, then run the retrospective.
func TestFixRebasesAHarnessBranchAndPushesForceWithLease(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(Conflict))

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a resolved, gate-green, CI-green conflict ships, got %+v", res)
	}
	got := calls(h)
	for _, want := range []string{"shell prep|", "agent rebasefix|", "shell gate-check|", "push-force|", "watch-ci|", "agent retrospective|"} {
		if !strings.Contains(got, want) {
			t.Errorf("calls missing %q:\n%v", want, h.Calls)
		}
	}
	if strings.Index(got, "shell gate-check") > strings.Index(got, "push-force") {
		t.Error("the gates must run before the push")
	}
	if strings.Contains(got, "|push|") || strings.Contains(got, "create-pr") {
		t.Errorf("a rebased branch is force-pushed to the existing PR, never pushed plain or re-opened: %v", h.Calls)
	}
	if !onMain(repo, "feat/gh-7") {
		t.Error("the resolved branch, now on origin/main, must be what reached origin")
	}
	if h.Worktree().Exists() {
		t.Error("a clean worktree is removed once the run is done: the branch on origin is the record")
	}
	if !strings.Contains(h.Agents[0].Prompt, "git reset --hard origin/main") {
		t.Error("a harness branch is resolved with the rebase recipe")
	}
	if h.Watches[0].KeepHistory {
		t.Error("the CI watch may rebase a harness branch reactively")
	}
}

// A conflict on a branch a human opted in: merge main into it and push without
// force, and forbid the CI watch from rewriting it either.
func TestFixMergesIntoAHumanBranchAndPushesWithoutForce(t *testing.T) {
	h, it, repo := prHost(t, humanItem(Conflict))

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK {
		t.Fatalf("a resolved, gate-green merge ships, got %+v", res)
	}
	got := calls(h)
	if !strings.Contains(got, "|push|") || strings.Contains(got, "push-force") {
		t.Errorf("a human branch is pushed without force; calls = %v", h.Calls)
	}
	if !strings.Contains(h.Agents[0].Prompt, "git merge --no-ff origin/main") || !strings.Contains(h.Agents[0].Prompt, "`alice/widget`") {
		t.Errorf("the session must merge main into alice/widget; prompt:\n%s", h.Agents[0].Prompt)
	}
	if !h.Watches[0].KeepHistory {
		t.Error("the CI watch must not rebase a human branch")
	}
	if !onMain(repo, "alice/widget") || !strings.Contains(repo.Git(repo.Origin, "log", "--format=%s", "alice/widget"), "a change on alice/widget") {
		t.Error("the merge, keeping the branch's own history, must have reached origin")
	}
}

// #266: the resolved conflict passed the gates and was pushed, then the CI watch
// could not read CI and ended not OK. The branch on origin is the resolution, so
// the run shipped: it is not a failed attempt against the PR's budget.
func TestFixShipsAPushedConflictResolutionWhoseCIWatchFails(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	h.CI = ci.Outcome{Reason: "polling CI failed: gh pr checks failed: exit status 1: error connecting to api.github.com"}

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !strings.Contains(calls(h), "push-force|watch-ci|") {
		t.Fatalf("the resolution must be pushed before the watch; calls = %v", h.Calls)
	}
	if res.Disposition != stages.Shipped {
		t.Fatalf("a pushed resolution ships whatever the CI watch says, got %+v", res)
	}
}

// Red CI: the watch re-runs the failed checks, then hands the failing logs to a
// CIFix session; the harness re-runs the gates over the committed fix before the
// watch may push it.
func TestFixRunsTheCIFixSessionAndGatesItsCommit(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: true, Reason: "green after the fix"}
	}

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if fixErr != nil {
		t.Fatalf("a committed, gate-green fix is accepted, got %v", fixErr)
	}
	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a fixed, green PR ships, got %+v", res)
	}
	if got := agentLabels(h); len(got) != 2 || got[0] != "cifix-1" || got[1] != "retrospective" {
		t.Errorf("agents = %v, want the CIFix session then the retrospective", got)
	}
	if !strings.Contains(h.Agents[0].Prompt, "FAIL TestThing") || !strings.Contains(h.Agents[0].Prompt, "`feat/gh-7`") {
		t.Error("the CIFix session gets the failing logs and the PR's branch")
	}
	if !strings.Contains(calls(h), "agent cifix-1|shell gate-cifix1-check|") {
		t.Errorf("the gates must run over the committed fix; calls = %v", h.Calls)
	}
	if got := repo.Git(repo.Dir, "log", "-1", "--format=%s", "feat/gh-7"); got != strings.SplitN(gitpkg.CIRerunMessage(), "\n", 2)[0] {
		t.Errorf("a session that committed nothing gets the re-trigger commit, got HEAD %q", got)
	}
}

// #186: the PR loop's slug is `pr-12`, but the worktree is on the PR's own head
// branch, `feat/gh-7`, cut by an earlier ticket run. The retrospective and the
// fix sessions name that branch, never a `feat/pr-12` that does not exist.
func TestFixSessionsNameThePRsHeadBranchNotOneBuiltFromTheSlug(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(CIRed))
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		_ = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: true}
	}
	cfg := pipelineCfg()
	cfg.Prompts.Retro = "/retrospective for {{.Identifier}} on branch `{{.Branch}}`."

	Fix(h, cfg, pipelineLog(t), it, prDoctor())

	if len(h.Agents) != 2 || h.Agents[1].Label != "retrospective" {
		t.Fatalf("agents = %v, want the CIFix session then the retrospective", agentLabels(h))
	}
	for _, a := range h.Agents {
		if !strings.Contains(a.Prompt, "`feat/gh-7`") || strings.Contains(a.Prompt, "feat/pr-12") {
			t.Errorf("%s prompt does not name the PR's head branch feat/gh-7:\n%s", a.Label, a.Prompt)
		}
	}
}

// A flake the watch's re-run washed out needs no session at all — and with no
// transcript there is nothing for a retrospective to mine.
func TestFixLaunchesNothingWhenTheRerunGoesGreen(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(CIRed))

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK || len(h.Agents) != 0 {
		t.Fatalf("a green re-run ships with no session, got %+v agents %v", res, agentLabels(h))
	}
}

// Ground truth, never the agent's word: a resolution that left the branch off its
// base pushes nothing.
func TestFixPushesNothingWhenTheResolutionFailsGroundTruth(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(Conflict))
	h.AgentFn = nil // the session resolves nothing

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if res.OK || res.Disposition != stages.NoPR {
		t.Fatalf("an unverified resolution is a no-PR run, got %+v", res)
	}
	assertNothingPushed(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
}

// A red gate over the resolved tree pushes nothing.
func TestFixPushesNothingWhenTheGatesFail(t *testing.T) {
	for _, it := range []Item{harnessItem(Conflict), humanItem(Conflict)} {
		h, it, repo := prHost(t, it)
		h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
			if strings.HasPrefix(s.Label, "gate") {
				return session.Outcome{ExitCode: 2}
			}
			return session.Outcome{}
		}

		res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

		if res.OK || res.Disposition != stages.NoPR {
			t.Fatalf("a red gate is a no-PR run, got %+v", res)
		}
		assertNothingPushed(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
	}
}

// A red gate over a CIFix commit refuses the fix, so the watch pushes nothing.
func TestFixRefusesACIFixWhoseGatesFail(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(CIRed))
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, "gate") {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL", true)
		return ci.Outcome{OK: false, Reason: "auto-fix session failed"}
	}

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if fixErr == nil || !strings.Contains(fixErr.Error(), `gate "check"`) {
		t.Fatalf("a fix over a red gate must be refused naming the gate, got %v", fixErr)
	}
	if res.OK || res.Disposition != stages.NoPR {
		t.Errorf("a refused fix pushed nothing, so the run is no-PR, got %+v", res)
	}
}

// redOnceMerged scripts every gate green over the branch alone and red over a
// tree that holds main's change (prHost's other.txt), writing failure as the red
// gate's step log: a fix that is green alone and red once merged (#259).
func redOnceMerged(t *testing.T, h *hostio.Fake, log *runlog.Logger, failure string) {
	t.Helper()
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if !strings.HasPrefix(s.Label, "gate") {
			return session.Outcome{}
		}
		if _, err := os.Stat(filepath.Join(s.WorktreePath, "other.txt")); err != nil {
			return session.Outcome{}
		}
		if err := os.WriteFile(filepath.Join(log.Dir, s.Label+".log"), []byte(failure), 0o644); err != nil {
			t.Fatal(err)
		}
		return session.Outcome{ExitCode: 1}
	}
}

// #259: red CI on the PR's merge ref, and a CIFix commit the gates pass on the
// branch alone but fail once merged with origin/main. The harness refuses it
// before the push, so nothing reaches origin, and hands the next CIFix session the
// merge gate's output instead of waiting a CI round for it. The trial merge's
// worktree is gone afterwards, and the branch never gained main's change.
func TestFixRefusesACIFixWhoseMergeWithMainFailsTheGates(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	log := pipelineLog(t)
	redOnceMerged(t, h, log, "FAIL other_test.go: TestOther: undefined: Widget\n")
	var errs []error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		errs = append(errs, w.Fix("FAIL TestThing", true), w.Fix("FAIL TestThing", true))
		return ci.Outcome{OK: false, Reason: "CI still red"}
	}

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	for i, err := range errs {
		if !errors.Is(err, ci.ErrFixRefused) || !strings.Contains(err.Error(), `gate "check"`) {
			t.Errorf("fix %d over a red merge gate is refused naming the gate, got %v", i+1, err)
		}
	}
	assertWatchPushedNothing(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
	if res.OK || res.Disposition != stages.NoPR {
		t.Errorf("a refused fix pushed nothing, so the run is no-PR, got %+v", res)
	}
	got := calls(h)
	if !strings.Contains(got, "agent cifix-1|shell gate-cifix1-check|") || !strings.Contains(got, "shell gate-cifix1-merge-check|") {
		t.Errorf("the branch gate, then the merge gate, run over the fix; calls = %v", h.Calls)
	}
	var cifix2 string
	for _, a := range h.Agents {
		if a.Label == "cifix-2" {
			cifix2 = a.Prompt
		}
	}
	if !strings.Contains(cifix2, "undefined: Widget") || !strings.Contains(cifix2, "origin/main") {
		t.Errorf("the next CIFix session gets the merge gate's output, got:\n%s", cifix2)
	}
	if strings.Contains(cifix2, "FAIL TestThing") {
		t.Error("the next CIFix session works the merge gate's red, not the CI log it already answered")
	}
	if _, err := os.Stat(h.Worktree().Path() + ".merge"); !os.IsNotExist(err) {
		t.Errorf("the trial merge's worktree is removed, stat err = %v", err)
	}
	if out := repo.Git(repo.Dir, "worktree", "list"); strings.Contains(out, ".merge") {
		t.Errorf("git still lists the trial merge:\n%s", out)
	}
}

// A fix whose merge with origin/main conflicts is refused too: there is no merged
// tree to gate, and CI cannot check one either.
func TestFixRefusesACIFixThatConflictsWithMain(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	repo.Merge("feat-gh-7.txt", "main's own version\n", "main edits the PR's file")
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: false, Reason: "auto-fix session failed"}
	}

	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if fixErr == nil || !strings.Contains(fixErr.Error(), "feat-gh-7.txt") {
		t.Fatalf("a fix that conflicts with main is refused naming the conflict, got %v", fixErr)
	}
	assertWatchPushedNothing(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
	if strings.Contains(calls(h), "merge-check") {
		t.Errorf("a conflicted merge has nothing to gate; calls = %v", h.Calls)
	}
}

// A branch that already holds origin/main has a merge ref identical to its HEAD,
// so the gates over the branch are the gates over the merge: none run twice.
func TestFixGatesABranchOnMainOnlyOnce(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "cifix-1" {
			repo.Resolve(h.Worktree())
		}
		return session.Outcome{}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: true}
	}

	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if fixErr != nil {
		t.Fatalf("a green fix on main is accepted, got %v", fixErr)
	}
	if strings.Contains(calls(h), "merge") {
		t.Errorf("no trial merge for a branch already on origin/main; calls = %v", h.Calls)
	}
}

// herd PR #1312 (#167): red CI on Build, a CIFix session that commits a fix, and
// the storybook gate over it red because the test browser's connection closed
// under a story the fix never touched. The gate is re-run once, and the fix it
// then passes is accepted rather than lost with the worktree. A red that names
// only files the fix left alone is re-run the same way; one in a file the fix
// changed is the fix's, and refuses it.
func TestFixRerunsAGateThatFailedForTheEnvironmentNotTheFix(t *testing.T) {
	const browserCrash = `Error: Failed to run the test /work/herd/web/src/routes/call/-components/more-tray.stories.tsx.
Caused by: Error: [vitest] Browser connection was closed while running tests. Was the page closed unexpectedly?
Caused by: Error: [birpc] rpc is closed, cannot call "createTesters"
`
	cases := []struct {
		name, log string
		accepted  bool
	}{
		{"the PR-1312 browser crash", browserCrash, true},
		{"a failure in a file the fix did not touch", "FAIL web/src/routes/call/-components/more-tray.stories.tsx > MoreTray\n", true},
		{"a failure in the file the fix changed", "FAIL web/vite.config.ts > config\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, it, repo := prHost(t, harnessItem(CIRed))
			log := pipelineLog(t)
			cfg := pipelineCfg()
			cfg.Gates = []config.Gate{{Name: "storybook", Command: "pnpm run test-storybook:changed"}}
			h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
				if run.Label == "cifix-1" {
					repo.Commit(h.Worktree(), "web/vite.config.ts", "export default {}\n", "Fix the RolldownError in Build")
				}
				return session.Outcome{}
			}
			gateRuns := 0
			h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
				if !strings.HasPrefix(s.Label, "gate-cifix1-storybook") {
					return session.Outcome{}
				}
				if gateRuns++; gateRuns > 1 && c.accepted {
					return session.Outcome{}
				}
				if err := os.WriteFile(filepath.Join(log.Dir, s.Label+".log"), []byte(c.log), 0o644); err != nil {
					t.Fatal(err)
				}
				return session.Outcome{ExitCode: 1}
			}
			var fixErr error
			h.CIFn = func(w hostio.CIWatch) ci.Outcome {
				fixErr = w.Fix("RolldownError: Build failed", true)
				return ci.Outcome{OK: fixErr == nil, Reason: "the fix's CI"}
			}

			res := Fix(h, cfg, log, it, prDoctor())

			if !c.accepted {
				if fixErr == nil || gateRuns != 1 {
					t.Fatalf("a red in the fix's own file refuses it with no re-run, got %v after %d gate runs", fixErr, gateRuns)
				}
				return
			}
			if fixErr != nil {
				t.Fatalf("the fix must be accepted once the re-run is green, got %v", fixErr)
			}
			if gateRuns != 2 || !strings.Contains(calls(h), "shell gate-cifix1-storybook|shell gate-cifix1-storybook-rerun|") {
				t.Errorf("want the gate then one re-run; calls = %v", h.Calls)
			}
			if !res.OK || res.Disposition != stages.Shipped {
				t.Errorf("an accepted fix ships, got %+v", res)
			}
		})
	}
}

// A push that landed between the poll and the checkout is someone else's newer
// head: the loop works what it polled or nothing.
func TestFixWorksOnlyThePolledHead(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(Conflict))
	published := it.PR.HeadSHA
	it.PR.HeadSHA = "fff9999" // the poll saw a head that has since moved on

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if res.OK || len(h.Agents) != 0 {
		t.Fatalf("a moved head is not worked, got %+v agents %v", res, agentLabels(h))
	}
	assertNothingPushed(t, h, repo, it.PR.HeadBranch, published)
}

// A spending cap that killed the session before it did anything is the daemon's
// back-off signal, not a failure to ship.
func TestFixSurfacesASpendingCap(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		return session.Outcome{ExitCode: 1, SpendingCapAbort: true}
	}

	if res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor()); res.Disposition != stages.CapAborted {
		t.Fatalf("a cap-aborted resolution is CapAborted, got %+v", res)
	}
}

// A host that cannot launch a sandbox is environmental: nothing is checked out.
func TestFixStopsAtAFailedPreflight(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	h.PreflightErr = errFake

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if res.Disposition != stages.PreflightAborted || h.Worktree().Exists() {
		t.Fatalf("a failed preflight aborts before the checkout, got %+v calls %v", res, h.Calls)
	}
}

// A loop without watch_ci stops at the push: the resolved branch ships, and CI is
// left to report on its own.
func TestFixWithoutWatchCIStopsAtThePush(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, withoutSteps(config.StepWatchCI))

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a pushed resolution ships, got %+v", res)
	}
	got := calls(h)
	if !strings.Contains(got, "push-force|") || strings.Contains(got, "watch-ci") {
		t.Errorf("want a push and no CI watch; calls = %v", h.Calls)
	}
}

// A loop without retro files no findings: no retrospective session runs.
func TestFixWithoutRetroRunsNoRetrospective(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))

	Fix(h, pipelineCfg(), pipelineLog(t), it, withoutSteps(config.StepRetro))

	if got := agentLabels(h); strings.Contains(strings.Join(got, ","), "retrospective") {
		t.Errorf("agents = %v, want no retrospective", got)
	}
}

// A fix step's prompt body is the Consumer's half of the RebaseFix and CIFix
// prompts, inside the same envelope.
func TestFixCarriesTheFixStepsPromptBody(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	cfg := pipelineCfg()
	cfg.Prompts.Fix = "Run `make lint` before you commit."

	Fix(h, cfg, pipelineLog(t), it, prDoctor())

	p := h.Agents[0].Prompt
	if !strings.Contains(p, "Run `make lint` before you commit.") || !strings.Contains(p, "git reset --hard origin/main") {
		t.Errorf("the RebaseFix prompt must carry the body inside its envelope:\n%s", p)
	}
}

// assertNothingPushed fails when a push or a watch was attempted, or origin's
// branch no longer sits at tip.
func assertNothingPushed(t *testing.T, h *hostio.Fake, repo *gittest.Repo, branch, tip string) {
	t.Helper()
	got := calls(h)
	if strings.Contains(got, "|push|") || strings.Contains(got, "push-force") || strings.Contains(got, "watch-ci") {
		t.Errorf("nothing may be pushed or watched; calls = %v", h.Calls)
	}
	if now := repo.OnOrigin(branch); now != tip {
		t.Errorf("origin's %s moved from %s to %s", branch, tip, now)
	}
}

// assertWatchPushedNothing fails when the CI watch pushed, or origin's branch no
// longer sits at tip.
func assertWatchPushedNothing(t *testing.T, h *hostio.Fake, repo *gittest.Repo, branch, tip string) {
	t.Helper()
	if got := calls(h); strings.Contains(got, "|push|") || strings.Contains(got, "push-force") {
		t.Errorf("nothing may be pushed; calls = %v", h.Calls)
	}
	if now := repo.OnOrigin(branch); now != tip {
		t.Errorf("origin's %s moved from %s to %s", branch, tip, now)
	}
}

// onMain reports whether origin's branch contains origin's main.
func onMain(repo *gittest.Repo, branch string) bool {
	return repo.Git(repo.Origin, "merge-base", "main", branch) == repo.Git(repo.Origin, "rev-parse", "main")
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errFake = fakeErr("docker unreachable")

// #174, replaying herd's PR-1312: with Linear as the tracker, the retrospective's
// project findings were dropped ("no team id resolved for PR-1312") because only
// a GitHub Issues tracker named a team for a PR. The tracker's findings team is
// where they file, whatever the tracker and whoever's branch the PR is on.
func TestFixFilesProjectFindingsIntoTheTrackersTeam(t *testing.T) {
	herdPR := func(it Item) Item {
		it.PR.Number = 1312
		return it
	}
	for name, it := range map[string]Item{
		"harness branch": herdPR(harnessItem(Conflict)),
		"human branch":   herdPR(humanItem(Conflict)),
	} {
		t.Run(name, func(t *testing.T) {
			h, it, _ := prHost(t, it)
			h.Trk.Team = "beh-team-uuid"
			cfg := pipelineCfg()
			cfg.Tracker = config.TrackerConfig{Kind: "linear", TeamKey: "BEH"}

			Fix(h, cfg, pipelineLog(t), it, prDoctor())

			if strings.Join(h.Routed, ",") != "PR-1312" || strings.Join(h.RoutedTeams, ",") != "beh-team-uuid" {
				t.Errorf("routed %v into teams %v, want PR-1312 into beh-team-uuid", h.Routed, h.RoutedTeams)
			}
		})
	}
}

// A tracker that cannot be resolved names no team, and the run goes on: filing is
// best-effort (ADR-0001).
func TestFixRunsTheRetrospectiveWhenTheTrackerIsUnresolvable(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	h.TrackerErr = errors.New("LINEAR_API_KEY is not set")

	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if strings.Join(h.RoutedTeams, ",") != "" || !strings.Contains(strings.Join(agentLabels(h), ","), "retrospective") {
		t.Errorf("agents %v routed into teams %q, want a retrospective with no team", agentLabels(h), h.RoutedTeams)
	}
}

// #247, replaying herd PR-1384 on 2026-11-07: the first attempt left its worktree
// dirty, so git refused to drop it on the next checkout. That refusal is the host's
// local state: no session launches, nothing is pushed, and the run is
// CheckoutBlocked, carrying the worktree and git's error, not a failure to ship.
func TestFixOverADirtyWorktreeIsCheckoutBlocked(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	wt := h.Worktree()
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("the first attempt's checkout: %v", err)
	}
	repo.Dirty(wt)

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	var local *gitpkg.LocalStateError
	if res.Disposition != stages.CheckoutBlocked || !errors.As(res.Err, &local) || local.Path != wt.Path() {
		t.Fatalf("a refused checkout is CheckoutBlocked on %s, got %+v", wt.Path(), res)
	}
	if !strings.Contains(res.Err.Error(), "modified or untracked files") {
		t.Errorf("the result must carry git's error: %v", res.Err)
	}
	if len(h.Agents) != 0 {
		t.Errorf("no session may launch, got %v", agentLabels(h))
	}
	assertNothingPushed(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
	if !wt.Exists() {
		t.Error("the dirty worktree must be left for a human to look at")
	}
}

// #254, the test #247 had to drop: the PR's head branch is gone from origin, so the
// checkout's fetch fails. That is the remote's doing, not the host's local state —
// no session, nothing pushed, and not CheckoutBlocked. Over gittest's local origin
// the fetch is tried once, so this takes as long as git does, not the retry budget.
func TestFixOnAHeadBranchGoneFromOriginLaunchesNothing(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	repo.Git(repo.Origin, "branch", "-q", "-D", it.PR.HeadBranch)
	start := time.Now()

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("a failed fetch over a local origin took %v: it waited out the retry budget", took)
	}
	if res.Disposition == stages.CheckoutBlocked || res.OK {
		t.Fatalf("a fetch that failed is neither CheckoutBlocked nor shipped, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("no session may launch, got %v", agentLabels(h))
	}
	if h.Worktree().Exists() {
		t.Error("no worktree is checked out for a branch origin does not have")
	}
}

// pr1384 is herd's PR-1384 as pr-doctor polled it on 2026-11-07: red CI on
// feat/beh-1198, a harness branch.
func pr1384() Item {
	it := harnessItem(CIRed)
	it.PR.Number, it.PR.Title, it.PR.HeadBranch = 1384, "BEH-1198: a story", "feat/beh-1198"
	return it
}

// capKilledCIFix scripts the incident's first attempt over h: the CIFix session
// writes a fix (the `waitFor` that hung the story runner), never commits it, and
// the 25-minute cap kills it (exit 137). The watch ends on the failed fix.
func capKilledCIFix(h *hostio.Fake, repo *gittest.Repo) {
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label != "cifix-1" {
			return session.Outcome{}
		}
		repo.Dirty(h.Worktree())
		return session.Outcome{ExitCode: 137, CapKilled: true}
	}
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		err := w.Fix("FAIL web/src/routes/call/-components/more-tray.stories.tsx > MoreTray", true)
		return ci.Outcome{OK: false, Reason: fmt.Sprintf("auto-fix session failed: %v", err)}
	}
}

// #246, replaying herd PR-1384 on 2026-11-07: CI-fix session 1 wrote a fix, did
// not commit it, and the cap killed it. The worktree was kept dirty, so attempts 2
// and 3 each failed to check the branch out and the PR was labelled blocked after
// one real attempt. Now the run saves the leftover diff as a local recovery
// checkpoint, pushes nothing, and leaves no dirty worktree, and the retry at the
// same head starts from the checkpoint and is told it is unverified.
func TestFixSavesACapKilledSessionsDiffAndTheRetryResumesFromIt(t *testing.T) {
	h, it, repo := prHost(t, pr1384())
	capKilledCIFix(h, repo)

	if res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor()); res.OK || res.Err != nil {
		t.Fatalf("a cap-killed fix ships nothing and needs no human, got %+v", res)
	}

	checkpoint := repo.Git(repo.Dir, "rev-parse", "refs/heads/feat/beh-1198")
	if got, want := repo.Git(repo.Dir, "log", "-1", "--format=%s", checkpoint), strings.SplitN(gitpkg.CheckpointMessage("PR-1384", who), "\n", 2)[0]; got != want {
		t.Fatalf("the local branch's tip is %q, want the recovery checkpoint %q", got, want)
	}
	if parent := repo.Git(repo.Dir, "rev-parse", checkpoint+"^"); parent != it.PR.HeadSHA {
		t.Errorf("the checkpoint sits on %s, want the polled head %s", parent, it.PR.HeadSHA)
	}
	if !strings.Contains(repo.Git(repo.Dir, "show", "--name-only", "--format=", checkpoint), "uncommitted.txt") {
		t.Error("the checkpoint must carry the session's uncommitted diff")
	}
	if now := repo.OnOrigin(it.PR.HeadBranch); now != it.PR.HeadSHA {
		t.Errorf("the checkpoint must not be pushed: origin moved to %s", now)
	}
	if wt := h.Worktree(); wt.Exists() && !wt.Clean() {
		t.Error("the run must not leave its worktree dirty")
	}

	// Attempt 2, 9 s later, at the same head.
	retry := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	retry.Branch = it.PR.HeadBranch
	var startedAt string
	retry.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "cifix-1" {
			startedAt, _ = retry.Worktree().HeadSHA()
		}
		return session.Outcome{}
	}
	retry.CIFn = func(w hostio.CIWatch) ci.Outcome {
		_ = w.Fix("FAIL web/src/routes/call/-components/more-tray.stories.tsx > MoreTray", true)
		return ci.Outcome{OK: true}
	}

	res := Fix(retry, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if res.Disposition == stages.CheckoutBlocked {
		t.Fatalf("the retry's checkout must succeed, got %+v", res)
	}
	if startedAt != checkpoint {
		t.Errorf("the retry's session started at %s, want the checkpoint %s", startedAt, checkpoint)
	}
	p := retry.Agents[0].Prompt
	for _, want := range []string{short(checkpoint), short(it.PR.HeadSHA), "UNVERIFIED"} {
		if !strings.Contains(p, want) {
			t.Errorf("the retry's prompt must name the checkpoint as unverified prior work (missing %q):\n%s", want, p)
		}
	}
}

// #246: when the leftover diff cannot be committed as a checkpoint (here git's
// index is locked), the next checkout would refuse the dirty worktree. The run
// says so at once, naming the worktree, so the PR goes to a human now instead of
// spending its remaining attempts on checkouts that cannot succeed.
func TestFixThatCannotSaveTheCheckpointNeedsAHuman(t *testing.T) {
	h, it, repo := prHost(t, pr1384())
	capKilledCIFix(h, repo)
	killed := h.AgentFn
	h.AgentFn = func(n int, run hostio.AgentRun) session.Outcome {
		out := killed(n, run)
		if run.Label == "cifix-1" {
			lock := filepath.Join(repo.Git(h.Worktree().Path(), "rev-parse", "--absolute-git-dir"), "index.lock")
			if err := os.WriteFile(lock, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	var unsaved *UnsavedError
	if res.OK || !errors.As(res.Err, &unsaved) || unsaved.Path != h.Worktree().Path() {
		t.Fatalf("an unsaved diff needs a human, naming the worktree %s, got %+v", h.Worktree().Path(), res)
	}
	if !strings.Contains(res.Err.Error(), h.Worktree().Path()) {
		t.Errorf("the reason must name the worktree: %v", res.Err)
	}
	if !h.Worktree().Exists() {
		t.Error("the dirty worktree must be kept for the human")
	}
	if now := repo.OnOrigin(it.PR.HeadBranch); now != it.PR.HeadSHA {
		t.Errorf("nothing may be pushed: origin moved to %s", now)
	}
}

// rebasingHost is prHost whose conflict-resolution session really rebases the
// branch onto origin/main, rewriting its history, as the RebaseFix recipe does on
// a harness branch, and whose gates are red while red is set.
func rebasingHost(t *testing.T, repo *gittest.Repo, it Item, red *bool) *hostio.Fake {
	t.Helper()
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			if res := h.Worktree().Rebase(); res != gitpkg.RebaseClean {
				t.Fatalf("the fixture's rebase must replay cleanly, got %v", res)
			}
		}
		return session.Outcome{}
	}
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if *red && strings.HasPrefix(s.Label, "gate") {
			return session.Outcome{ExitCode: 1}
		}
		return session.Outcome{}
	}
	return h
}

// #252: a conflict-resolution session rebased and committed the branch cleanly,
// the re-gate went red for a reason outside the branch, and the run logged
// "keeping worktree, nothing pushed" — then removed the clean worktree, and the
// next run's checkout reset the branch to the PR's head, so a second RebaseFix
// session redid the same resolution from scratch. Now the run saves the resolved
// tip, and the next run at the same head, on the same origin/main, re-gates and
// pushes it without a session.
func TestFixKeepsACommittedResolutionWhoseRegateWentRed(t *testing.T) {
	repo := gittest.New(t)
	it := harnessItem(Conflict)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	red := true
	first := rebasingHost(t, repo, it, &red)
	log := pipelineLog(t)

	if res := Fix(first, pipelineCfg(), log, it, prDoctor()); res.OK {
		t.Fatalf("a red re-gate pushes nothing, got %+v", res)
	}
	assertNothingPushed(t, first, repo, it.PR.HeadBranch, it.PR.HeadSHA)
	events := readEvents(t, log)
	if strings.Contains(events, "keeping worktree") && !first.Worktree().Exists() {
		t.Errorf("the run log must not say the worktree is kept when cleanup removed it:\n%s", events)
	}
	resolved := repo.Git(repo.Dir, "rev-parse", "refs/harness/resolved/pr-12/"+it.PR.HeadSHA)
	if !onMainAt(repo, resolved) {
		t.Fatalf("the saved resolution %s must sit on origin/main", resolved)
	}

	red = false
	retry := rebasingHost(t, repo, it, &red)
	res := Fix(retry, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("the saved resolution, green now, ships, got %+v", res)
	}
	for _, l := range agentLabels(retry) {
		if l == "rebasefix" {
			t.Fatalf("no second conflict-resolution session may run over a saved resolution whose base is unchanged, got %v", agentLabels(retry))
		}
	}
	got := calls(retry)
	if !strings.Contains(got, "shell gate-check") || !strings.Contains(got, "push-force|") {
		t.Errorf("the saved resolution is re-gated and force-pushed: %v", retry.Calls)
	}
	if now := repo.OnOrigin(it.PR.HeadBranch); now != resolved {
		t.Errorf("origin must have the saved resolution %s, got %s", resolved, now)
	}
	if out := repo.Git(repo.Dir, "for-each-ref", "refs/harness/resolved/"); out != "" {
		t.Errorf("a pushed resolution leaves no saved ref behind: %s", out)
	}
}

// #252: a saved resolution is cut from the origin/main it was resolved onto. Once
// origin/main moves on, it is no longer a resolution of the conflict the PR has,
// so the next run resolves afresh.
func TestFixResolvesAfreshOnceOriginMainMovesPastASavedResolution(t *testing.T) {
	repo := gittest.New(t)
	it := harnessItem(Conflict)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	red := true
	if res := Fix(rebasingHost(t, repo, it, &red), pipelineCfg(), pipelineLog(t), it, prDoctor()); res.OK {
		t.Fatalf("a red re-gate pushes nothing, got %+v", res)
	}
	repo.Merge("another.txt", "more\n", "main moved on again")

	red = false
	retry := rebasingHost(t, repo, it, &red)
	res := Fix(retry, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK {
		t.Fatalf("a fresh resolution ships, got %+v", res)
	}
	if labels := agentLabels(retry); len(labels) == 0 || labels[0] != "rebasefix" {
		t.Errorf("a saved resolution on a stale origin/main must be resolved afresh, got %v", labels)
	}
	if !onMain(repo, it.PR.HeadBranch) {
		t.Error("what reached origin must sit on the current origin/main")
	}
}

// onMainAt reports whether origin/main, as the checkout has it, is an ancestor of rev.
func onMainAt(repo *gittest.Repo, rev string) bool {
	_, err := exec.Command("git", "-C", repo.Dir, "merge-base", "--is-ancestor", "origin/main", rev).Output()
	return err == nil
}

// readEvents is the run log's narration so far.
func readEvents(t *testing.T, log *runlog.Logger) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(log.Dir, "run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// #250, replayed: the session resolved the conflict cleanly onto an origin/main
// that does not build by itself, so the re-gate went red on the base's compile
// error. The run is not the PR's failure: it holds on that base as a BaseRed run
// until main moves, files the base-gates-red finding, and pushes nothing.
func TestFixHoldsAResolutionRedOnlyBecauseTheBaseIsRed(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(Conflict))
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, "gate") || strings.HasPrefix(s.Label, "basegate") {
			return session.Outcome{ExitCode: 2, StderrTail: "internal/fake/fake.go:14:9: *Fake does not implement Port (missing method Close)"}
		}
		return session.Outcome{}
	}
	cfg := pipelineCfg()
	cfg.ProjectPath = t.TempDir()
	base := repo.OnOrigin("main")
	before := time.Now()
	log := pipelineLog(t)

	res := Fix(h, cfg, log, it, prDoctor())

	if res.OK || res.Disposition != stages.BaseRed {
		t.Fatalf("a re-gate red only because the base is red is a BaseRed hold, got %+v", res)
	}
	if res.HeldBase != base {
		t.Errorf("held on %q, want origin/main %q", res.HeldBase, base)
	}
	if until := res.HeldUntil; until.Before(before.Add(basegate.RedRecheck)) || until.After(time.Now().Add(basegate.RedRecheck)) {
		t.Errorf("held until %v, want the recorded verdict's expiry", until)
	}
	if strings.Join(h.Filed, ",") != Key(it.PR.Number) {
		t.Errorf("filed = %v, want the red-base finding filed once for %s", h.Filed, Key(it.PR.Number))
	}
	if dropbox, _ := os.ReadFile(filepath.Join(log.FindingsDir("basegate"), "out.json")); !strings.Contains(string(dropbox), basegate.FindingKey) {
		t.Errorf("findings dropbox = %s, want the %s finding", dropbox, basegate.FindingKey)
	}
	assertNothingPushed(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
}

// adrHost is prHost for #267: the PR adds ADR-0040, main then merges an unrelated
// ADR-0040, and the conflict-resolution session rebases onto main and renumbers
// the branch's ADR to 0041, as the ADR rules require (ADR-0034).
func adrHost(t *testing.T, it Item) (*hostio.Fake, Item) {
	t.Helper()
	repo := gittest.New(t)
	it.PR.HeadSHA = repo.PublishFile(it.PR.HeadBranch, "docs/adr/0040-a-pr-records-its-decision.md", "# ADR-0040: a PR records its decision\n")
	repo.Merge("docs/adr/0040-main-decided-first.md", "# ADR-0040: main decided first\n", "a sibling's ADR")
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			wt := h.Worktree()
			repo.Git(wt.Path(), "reset", "-q", "--hard", "origin/main")
			repo.Commit(wt, "docs/adr/0041-a-pr-records-its-decision.md", "# ADR-0041: a PR records its decision\n", "Record the decision (ADR-0041)")
		}
		return h.AgentOutcome
	}
	return h, it
}

// #267, replayed: the session renumbered the PR's ADR-0040 to 0041 and the gates
// passed, but the harness only pushed the branch, so the PR's title and
// description still cited ADR-0040, which now names main's unrelated decision.
// After the push the pr-fix stage rewrites them itself: the renumbered ADR's
// references and its file's link move to 0041, and an ADR the resolution did not
// renumber is left alone.
func TestFixRewritesThePRDescriptionWhenTheResolutionRenumbersAnADR(t *testing.T) {
	it := harnessItem(Conflict)
	it.PR.Title = "#7: Record the decision in ADR-0040"
	it.PR.Body = "Adds ADR-0040 ([docs/adr/0040-a-pr-records-its-decision.md](docs/adr/0040-a-pr-records-its-decision.md)), which extends ADR-0034.\n\nCloses #7"
	h, it := adrHost(t, it)

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a resolved, gate-green conflict ships, got %+v", res)
	}
	want := []hostio.PREdit{{
		Number: 12,
		Title:  "#7: Record the decision in ADR-0041",
		Body:   "Adds ADR-0041 ([docs/adr/0041-a-pr-records-its-decision.md](docs/adr/0041-a-pr-records-its-decision.md)), which extends ADR-0034.\n\nCloses #7",
	}}
	if !reflect.DeepEqual(h.PREdits, want) {
		t.Errorf("PR edits = %+v, want %+v", h.PREdits, want)
	}
	got := calls(h)
	if strings.Index(got, "edit-pr") < strings.Index(got, "push-force") {
		t.Errorf("the PR is edited after the push that carries the renumber; calls = %v", h.Calls)
	}
}

// A resolution that renumbers nothing leaves the PR's description alone, and so
// does one that renumbers an ADR the description never cites.
func TestFixLeavesThePRDescriptionAloneWithoutARenumberItCites(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())
	if strings.Contains(calls(h), "edit-pr") {
		t.Errorf("no ADR was renumbered, so the PR is not edited; calls = %v", h.Calls)
	}

	h, it = adrHost(t, harnessItem(Conflict))
	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())
	if strings.Contains(calls(h), "edit-pr") {
		t.Errorf("the description cites no renumbered ADR, so the PR is not edited; calls = %v", h.Calls)
	}
}

// The branch is already pushed when the edit fails, so the run still ships: a
// stale description is narrated, not a failed attempt.
func TestFixShipsWhenThePRDescriptionCannotBeEdited(t *testing.T) {
	it := harnessItem(Conflict)
	it.PR.Body = "Adds ADR-0040."
	h, it := adrHost(t, it)
	h.EditPRErr = errors.New("gh: HTTP 502")

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a pushed resolution ships even when its description cannot be fixed, got %+v", res)
	}
}

// Two renumbers that chain, 0040 → 0041 while 0041 → 0042, move each reference
// once rather than carrying ADR-0040 on to 0042.
func TestRenumberADRsMovesEachReferenceOnce(t *testing.T) {
	moved := []gitpkg.Renumber{
		{From: "0040", To: "0041", FromFile: "0040-a.md", ToFile: "0041-a.md"},
		{From: "0041", To: "0042", FromFile: "0041-b.md", ToFile: "0042-b.md"},
	}
	got := renumberADRs("ADR-0040 (0040-a.md) and ADR-0041 (0041-b.md), not ADR-00401 or ADR-0012", moved)
	if want := "ADR-0041 (0041-a.md) and ADR-0042 (0042-b.md), not ADR-00401 or ADR-0012"; got != want {
		t.Errorf("renumberADRs = %q, want %q", got, want)
	}
}
