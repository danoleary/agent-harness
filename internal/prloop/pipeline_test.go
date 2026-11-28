package prloop

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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
	"github.com/danoleary/agent-harness/internal/runlog/runlogtest"
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
	return runlogtest.New(t, "PR-12")
}

// prHost is a healthy Fake for it's PR over a real repo: the PR's head branch is on
// origin, main has moved on past where it was cut, and it is polled at the head
// origin really has. A Conflict PR's main changed the branch's own file, so git
// stops on it too and the host cannot replay it (#403). A conflict-resolution
// session really brings the branch onto origin/main; every other session does
// what NewFake scripts.
func prHost(t *testing.T, it Item) (*hostio.Fake, Item, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	if it.Kind == Conflict {
		conflictWithMain(repo, it.PR.HeadBranch)
	} else {
		repo.Merge("other.txt", "unrelated\n", "main moved on")
	}
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

// conflictWithMain lands on main a different edit to the file [gittest.Repo.Publish]
// wrote for branch, so the branch's replay onto origin/main stops on it.
func conflictWithMain(repo *gittest.Repo, branch string) {
	repo.Merge(strings.ReplaceAll(branch, "/", "-")+".txt", "main's change\n", "main changed the same file")
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
	if !strings.Contains(h.Agents[0].Prompt, "genuine content conflict: git left `feat-gh-7.txt` unmerged") {
		t.Errorf("the session is told the paths the host's replay left unmerged (#403); prompt:\n%s", h.Agents[0].Prompt)
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

// #383: a watch that could not run gh never read CI, so the run log says the
// harness could not poll CI, not that CI went red.
func TestFixNarratesAPollFaultApartFromARedCI(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	h.CI = ci.Outcome{PollFault: true, Reason: "CI watch could not run: the gh CLI is not on the harness PATH"}
	log := pipelineLog(t)

	Fix(h, pipelineCfg(), log, it, prDoctor())

	events := readEvents(t, log)
	if !strings.Contains(events, "could not poll CI (not a red CI): CI watch could not run") {
		t.Errorf("the run log must name the poll fault; events:\n%s", events)
	}
	if strings.Contains(events, "did not go green") {
		t.Errorf("a poll fault must not read as a red CI; events:\n%s", events)
	}
}

// Red CI: the watch re-runs the failed checks, then hands the failing logs to a
// CIFix session; the harness re-runs the gates over the committed fix before the
// watch may push it.
func TestFixRunsTheCIFixSessionAndGatesItsCommit(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "cifix-1" {
			repo.Commit(h.Worktree(), "thing.go", "package thing\n", "Fix TestThing")
		}
		return session.Outcome{}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
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
}

// #396: a CIFix session that committed nothing gets the re-trigger commit, and
// its unchanged tree is not gated on the branch again. origin/main has moved past
// the branch, so the merge CI checks is still gated.
func TestFixReTriggersCIOverASessionThatCommittedNothingWithoutReGatingTheBranch(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
		return ci.Outcome{OK: true, Reason: "green after the re-trigger"}
	}

	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if fixErr != nil {
		t.Fatalf("a re-trigger is accepted, got %v", fixErr)
	}
	if got := repo.Git(repo.Dir, "log", "-1", "--format=%s", "feat/gh-7"); got != strings.SplitN(gitpkg.CIRerunMessage(), "\n", 2)[0] {
		t.Errorf("a session that committed nothing gets the re-trigger commit, got HEAD %q", got)
	}
	if got := calls(h); strings.Contains(got, "gate-cifix1-check") || !strings.Contains(got, "shell gate-cifix1-merge-check|") {
		t.Errorf("only the merge gate runs over a fix that changed nothing; calls = %v", h.Calls)
	}
}

// #186: the PR loop's slug is `pr-12`, but the worktree is on the PR's own head
// branch, `feat/gh-7`, cut by an earlier ticket run. The retrospective and the
// fix sessions name that branch, never a `feat/pr-12` that does not exist.
func TestFixSessionsNameThePRsHeadBranchNotOneBuiltFromTheSlug(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(CIRed))
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		_ = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
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
		fixErr = w.Fix(ci.FixInput{Logs: "FAIL", LogAvailable: true})
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
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if strings.HasPrefix(run.Label, "cifix-") {
			repo.Commit(h.Worktree(), "thing.go", "package thing // "+run.Label+"\n", "Fix TestThing")
		}
		return session.Outcome{}
	}
	var errs []error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		errs = append(errs, w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true}), w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true}))
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
		fixErr = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
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
		fixErr = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
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
				fixErr = w.Fix(ci.FixInput{Logs: "RolldownError: Build failed", LogAvailable: true})
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
		err := w.Fix(ci.FixInput{Logs: "FAIL web/src/routes/call/-components/more-tray.stories.tsx > MoreTray", LogAvailable: true})
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
		_ = w.Fix(ci.FixInput{Logs: "FAIL web/src/routes/call/-components/more-tray.stories.tsx > MoreTray", LogAvailable: true})
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
// a harness branch, and whose gates are red while red is set. The session
// resolves the conflict [conflictWithMain] made by keeping the branch's side.
func rebasingHost(t *testing.T, repo *gittest.Repo, it Item, red *bool) *hostio.Fake {
	t.Helper()
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			wt := h.Worktree()
			repo.Git(wt.Path(), "reset", "-q", "--hard", "origin/main")
			repo.Commit(wt, strings.ReplaceAll(it.PR.HeadBranch, "/", "-")+".txt", "a change\n", "a change on "+it.PR.HeadBranch)
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

// #472: a saved resolution re-gated under memory pressure is SIGKILLed on its run
// and its re-run. Nothing ships, and the run log names the double kill as infra,
// not a failed gate over the resolution.
func TestASavedResolutionKilledTwiceOnItsRegateIsInfra(t *testing.T) {
	repo := gittest.New(t)
	it := harnessItem(Conflict)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	conflictWithMain(repo, it.PR.HeadBranch)
	red := true
	if res := Fix(rebasingHost(t, repo, it, &red), pipelineCfg(), pipelineLog(t), it, prDoctor()); res.OK {
		t.Fatalf("a red re-gate pushes nothing, got %+v", res)
	}

	retry := rebasingHost(t, repo, it, &red)
	retry.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, "gate") {
			return session.Outcome{ExitCode: 2, StderrTail: "golangci-lint: signal: killed"}
		}
		return session.Outcome{}
	}
	log := pipelineLog(t)
	if res := Fix(retry, pipelineCfg(), log, it, prDoctor()); res.OK {
		t.Fatalf("a re-gate killed twice pushes nothing, got %+v", res)
	}
	events := readEvents(t, log)
	if !strings.Contains(events, "over the saved resolution is red twice with a SIGKILL") || strings.Contains(events, "saved resolution failed") {
		t.Errorf("the run log must name the double kill as infra:\n%s", events)
	}
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
	conflictWithMain(repo, it.PR.HeadBranch)
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

// #479, replayed: a resolution saved at a PR head, whose re-gate went red, was
// cut from an origin/main that then moved on by a commit the resolution does not
// touch. The next run at that head used to drop it and launch a second
// conflict-resolution session that re-derived the same hunks from nothing. Now it
// says it found the saved resolution on an old base, replays it onto origin/main
// as it stands, re-gates it and pushes it, with no session.
func TestFixReplaysASavedResolutionOntoAMovedOriginMain(t *testing.T) {
	repo := gittest.New(t)
	it := harnessItem(Conflict)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	conflictWithMain(repo, it.PR.HeadBranch)
	red := true
	if res := Fix(rebasingHost(t, repo, it, &red), pipelineCfg(), pipelineLog(t), it, prDoctor()); res.OK {
		t.Fatalf("a red re-gate pushes nothing, got %+v", res)
	}
	saved := repo.Git(repo.Dir, "rev-parse", "refs/harness/resolved/pr-12/"+it.PR.HeadSHA)
	repo.Merge("another.txt", "more\n", "main moved on again")

	red = false
	retry := rebasingHost(t, repo, it, &red)
	log := pipelineLog(t)
	res := Fix(retry, pipelineCfg(), log, it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("the saved resolution, replayed onto the new base and green, ships, got %+v", res)
	}
	if labels := agentLabels(retry); slices.Contains(labels, "rebasefix") {
		t.Errorf("a saved resolution that replays cleanly onto the moved origin/main needs no session, got %v", labels)
	}
	if !onMain(repo, it.PR.HeadBranch) {
		t.Error("what reached origin must sit on the current origin/main")
	}
	if pushed := repo.OnOrigin(it.PR.HeadBranch); repo.Git(repo.Dir, "show", pushed+":feat-gh-7.txt") != "a change" {
		t.Error("the push must carry the saved resolution's side of the conflict")
	}
	events := readEvents(t, log)
	if !strings.Contains(events, "found the saved conflict resolution "+short(saved)) || !strings.Contains(events, "replaying it onto origin/main") {
		t.Errorf("the run log must say it found the saved resolution and is replaying it:\n%s", events)
	}
	if out := repo.Git(repo.Dir, "for-each-ref", "refs/harness/resolved/"); out != "" {
		t.Errorf("a pushed resolution leaves no saved ref behind: %s", out)
	}
}

// #479: when origin/main's new commits conflict with the saved resolution too,
// its replay stops. The run says it set the saved resolution aside and why, and
// the conflict-resolution session is handed it as a prior resolution to reuse.
func TestFixHandsASavedResolutionThatNoLongerReplaysToTheSession(t *testing.T) {
	repo := gittest.New(t)
	it := harnessItem(Conflict)
	it.PR.HeadSHA = repo.Publish(it.PR.HeadBranch)
	conflictWithMain(repo, it.PR.HeadBranch)
	red := true
	if res := Fix(rebasingHost(t, repo, it, &red), pipelineCfg(), pipelineLog(t), it, prDoctor()); res.OK {
		t.Fatalf("a red re-gate pushes nothing, got %+v", res)
	}
	saved := repo.Git(repo.Dir, "rev-parse", "refs/harness/resolved/pr-12/"+it.PR.HeadSHA)
	repo.Merge("feat-gh-7.txt", "main changed it again\n", "main changed the same file again")

	red = false
	retry := rebasingHost(t, repo, it, &red)
	log := pipelineLog(t)
	res := Fix(retry, pipelineCfg(), log, it, prDoctor())

	if !res.OK {
		t.Fatalf("the session's fresh resolution ships, got %+v", res)
	}
	if labels := agentLabels(retry); len(labels) == 0 || labels[0] != "rebasefix" {
		t.Fatalf("a saved resolution that does not replay goes to a session, got %v", labels)
	}
	if p := retry.Agents[0].Prompt; !strings.Contains(p, "PRIOR RESOLUTION") || !strings.Contains(p, short(saved)) {
		t.Errorf("the session must be handed the saved resolution %s to reuse:\n%s", short(saved), p)
	}
	events := readEvents(t, log)
	if !strings.Contains(events, "does not replay onto origin/main") || !strings.Contains(events, "feat-gh-7.txt") {
		t.Errorf("the run log must say why the saved resolution was not reused:\n%s", events)
	}
	if !onMain(repo, it.PR.HeadBranch) {
		t.Error("what reached origin must sit on the current origin/main")
	}
}

// #479: a conflict run with no saved resolution says so, so the operator can tell
// the reuse path ran and found nothing.
func TestFixSaysWhenThereIsNoSavedResolution(t *testing.T) {
	h, it, _ := prHost(t, harnessItem(Conflict))
	log := pipelineLog(t)

	Fix(h, pipelineCfg(), log, it, prDoctor())

	if events := readEvents(t, log); !strings.Contains(events, "no saved conflict resolution of "+short(it.PR.HeadSHA)) {
		t.Errorf("the run log must say no saved resolution was found:\n%s", events)
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

// PR-217 (#221): a conflict whose resolved diff against origin/main is one
// workflow file. The PR loop skips the gate whose skip_when_only covers it, runs
// the one that has no list, and pushes; CI checks the rest before merge.
func TestFixSkipsAGateWhoseSkipWhenOnlyCoversTheWholeDiff(t *testing.T) {
	it := harnessItem(Conflict)
	repo := gittest.New(t)
	it.PR.HeadSHA = repo.PublishFile(it.PR.HeadBranch, ".github/workflows/ci.yml", "uses: actions/checkout@v4.3.0\n")
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			repo.Resolve(h.Worktree())
		}
		return h.AgentOutcome
	}
	cfg := pipelineCfg()
	cfg.Gates = []config.Gate{
		{Name: "check", Command: "make check", SkipWhenOnly: []string{".github/**"}},
		{Name: "build", Command: "make build"},
	}
	log := pipelineLog(t)

	res := Fix(h, cfg, log, it, prDoctor())

	if !res.OK {
		t.Fatalf("a resolved workflow-only conflict ships, got %+v", res)
	}
	got := calls(h)
	if strings.Contains(got, "shell gate-check|") {
		t.Errorf("gate check must be skipped over a .github-only diff: %v", h.Calls)
	}
	if !strings.Contains(got, "shell gate-build|") || !strings.Contains(got, "push-force|") {
		t.Errorf("gate build runs and the resolution is pushed: %v", h.Calls)
	}
	if ev := readEvents(t, log); !strings.Contains(ev, `gate \"check\" skipped`) {
		t.Errorf("the run log must say check was skipped; got %s", ev)
	}
}

// overlapHost is prHost whose conflict-resolution session also writes report as
// its overlap file (#255).
func overlapHost(t *testing.T, it Item, report string) (*hostio.Fake, Item) {
	t.Helper()
	h, it, repo := prHost(t, it)
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			repo.Resolve(h.Worktree())
			if run.FindingsDir == "" {
				t.Fatal("the replay session was given no /findings dropbox to report an overlap in")
			}
			if err := os.WriteFile(filepath.Join(run.FindingsDir, "overlaps.json"), []byte(report), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return h.AgentOutcome
	}
	return h, it
}

// #344, replayed: the PR loop's replay of an open PR reported that the rebased
// branch duplicates a mechanism main added while it was in flight, and both now
// fire on the same run. The host narrated it in run.jsonl only, pushed and watched
// CI green, and the human merging the PR never saw it. Now the pushed resolution
// puts the report on the PR itself: an `## Overlap with main` section in its
// description carrying each reported line, above the harness trailer, and one a
// previous replay left there is replaced, not stacked.
func TestFixPutsTheReplaysOverlapReportOnTheOpenPR(t *testing.T) {
	report := `[{"file": "internal/fix/fix.go", "main_added": "the already-resolved-on-main Disposition (ADR-0052)", "branch_duplicates": "its own resolved.json short-circuit in the tdd stage"}, {"file": "internal/stages/tdd.go", "main_added": "verify.AlreadyResolved", "branch_duplicates": "a second already-on-main check"}]`
	it := harnessItem(Conflict)
	it.PR.Body = "Resolves #7.\n\n## Commits\n\n- a commit\n\n## Overlap with `main`\n\nAn earlier replay's report:\n\n- `stale.go`: `main` added x; the branch duplicates it with y.\n\n---\n\n🤖 trailer\n"
	h, it := overlapHost(t, it, report)

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a reported overlap does not hold the push, got %+v", res)
	}
	if len(h.PREdits) != 1 {
		t.Fatalf("want one PR edit carrying the overlap report, got %+v", h.PREdits)
	}
	edit := h.PREdits[0]
	if edit.Number != 12 || edit.Title != it.PR.Title {
		t.Errorf("the edit keeps the PR's number and title, got %+v", edit)
	}
	for _, want := range []string{
		"## Overlap with `main`",
		"`internal/fix/fix.go`: `main` added the already-resolved-on-main Disposition (ADR-0052); the branch duplicates it with its own resolved.json short-circuit in the tdd stage.",
		"`internal/stages/tdd.go`: `main` added verify.AlreadyResolved; the branch duplicates it with a second already-on-main check.",
	} {
		if !strings.Contains(edit.Body, want) {
			t.Errorf("PR body missing %q:\n%s", want, edit.Body)
		}
	}
	if n := strings.Count(edit.Body, "## Overlap with `main`"); n != 1 || strings.Contains(edit.Body, "stale.go") {
		t.Errorf("the section replaces the earlier replay's rather than stacking on it:\n%s", edit.Body)
	}
	if !strings.HasPrefix(edit.Body, "Resolves #7.\n\n## Commits\n\n- a commit\n\n") || !strings.HasSuffix(edit.Body, "\n---\n\n🤖 trailer\n") {
		t.Errorf("the rest of the description is kept, the trailer last:\n%s", edit.Body)
	}
	got := calls(h)
	if strings.Index(got, "edit-pr") < strings.Index(got, "push-force") {
		t.Errorf("the PR is edited after the push that carries the resolution; calls = %v", h.Calls)
	}
}

// A replay that reports no overlap leaves the PR's description alone.
func TestFixLeavesThePRAloneWhenTheReplayReportsNoOverlap(t *testing.T) {
	h, it := overlapHost(t, harnessItem(Conflict), "[]")
	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())
	if strings.Contains(calls(h), "edit-pr") {
		t.Errorf("no overlap was reported, so the PR is not edited; calls = %v", h.Calls)
	}
}

// adrClashHost is the #403 incident: the ADR index merges by line union
// (.gitattributes), the PR adds ADR 0040 and its index line, and main then merges a
// sibling's ADR 0040, whose index line lands at the same spot. GitHub, which
// ignores merge=union, reads the PR as conflicting; a local replay applies clean.
func adrClashHost(t *testing.T, it Item) (*hostio.Fake, Item, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	index := "# Architecture decision records\n\n- [ADR 0039](0039-an-earlier-decision.md): An earlier decision\n"
	repo.MergeFiles(map[string]string{
		".gitattributes":                       "docs/adr/README.md merge=union\n",
		"docs/adr/README.md":                   index,
		"docs/adr/0039-an-earlier-decision.md": "# ADR 0039: an earlier decision\n",
	}, "the ADR index unions")
	it.PR.HeadSHA = repo.PublishFiles(it.PR.HeadBranch, map[string]string{
		"docs/adr/0040-a-pr-records-its-decision.md": "# ADR 0040: a PR records its decision\n",
		"docs/adr/README.md":                         index + "- [ADR 0040](0040-a-pr-records-its-decision.md): A PR records its decision\n",
	})
	repo.MergeFiles(map[string]string{
		"docs/adr/0040-main-decided-first.md": "# ADR 0040: main decided first\n",
		"docs/adr/README.md":                  index + "- [ADR 0040](0040-main-decided-first.md): Main decided first\n",
	}, "a sibling's ADR")
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	return h, it, repo
}

// #403, replayed: GitHub flagged the PR conflicting only because main and the
// branch each added an ADR under one number. The run replays the branch onto
// origin/main host-side, renumbers the branch's ADR, re-gates and force-pushes,
// with no conflict-resolution session.
func TestFixReplaysAGitHubOnlyConflictHostSideAndRenumbersTheClashingADR(t *testing.T) {
	it := harnessItem(Conflict)
	it.PR.Title = "#7: Record the decision in ADR-0040"
	h, it, repo := adrClashHost(t, it)
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a branch that replays clean ships, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("a replay that applies clean host-side launches no session, got %v", agentLabels(h))
	}
	got := calls(h)
	if !strings.Contains(got, "shell gate-check|") || strings.Index(got, "shell gate-check") > strings.Index(got, "push-force|") {
		t.Errorf("the replayed branch is re-gated, then force-pushed: %v", h.Calls)
	}
	if !onMain(repo, it.PR.HeadBranch) {
		t.Error("what reached origin must sit on origin/main")
	}
	files := repo.Git(repo.Origin, "ls-tree", "--name-only", it.PR.HeadBranch, "docs/adr/")
	if !strings.Contains(files, "0041-a-pr-records-its-decision.md") || strings.Contains(files, "0040-a-pr-records-its-decision.md") {
		t.Errorf("the branch's ADR must be renumbered past main's 0040, got:\n%s", files)
	}
	readme := repo.Git(repo.Origin, "show", it.PR.HeadBranch+":docs/adr/README.md")
	if !strings.Contains(readme, "(0040-main-decided-first.md)") || !strings.Contains(readme, "(0041-a-pr-records-its-decision.md)") || strings.Contains(readme, "(0040-a-pr-records-its-decision.md)") {
		t.Errorf("the index must keep main's line and carry the branch's under its new number, got:\n%s", readme)
	}
	if len(h.PREdits) != 1 || h.PREdits[0].Title != "#7: Record the decision in ADR-0041" {
		t.Errorf("the PR title follows the renumber, got %+v", h.PREdits)
	}
	if events := readEvents(t, log); strings.Contains(events, "launching conflict-resolution session") || !strings.Contains(events, "replayed") {
		t.Errorf("the run log must say the host replayed the branch, not that a session resolved a conflict:\n%s", events)
	}
}

// #403: a replay that applies clean host-side but whose re-gate is red is held
// like a session's resolution (#252): saved, not pushed, and no session runs.
func TestFixSavesAHostReplayWhoseRegateWentRed(t *testing.T) {
	h, it, repo := adrClashHost(t, harnessItem(Conflict))
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, "gate") {
			return session.Outcome{ExitCode: 1}
		}
		return session.Outcome{}
	}

	if res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor()); res.OK {
		t.Fatalf("a red re-gate pushes nothing, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("a clean host replay launches no session, red gate or not, got %v", agentLabels(h))
	}
	assertNothingPushed(t, h, repo, it.PR.HeadBranch, it.PR.HeadSHA)
	if saved := repo.Git(repo.Dir, "rev-parse", "refs/harness/resolved/pr-12/"+it.PR.HeadSHA); !onMainAt(repo, saved) {
		t.Errorf("the replayed tip %s is saved on origin/main for the next run", saved)
	}
}

// A human's branch keeps its history (ADR-0019), so the host never replays it:
// its session merges, and is told only GitHub reported the conflict (#403).
func TestFixLeavesAHumanBranchsGitHubOnlyConflictToTheMergeSession(t *testing.T) {
	h, it, _ := adrClashHost(t, humanItem(Conflict))

	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if labels := agentLabels(h); len(labels) == 0 || labels[0] != "rebasefix" {
		t.Fatalf("a human branch's conflict goes to the merge session, got %v", labels)
	}
	if p := h.Agents[0].Prompt; !strings.Contains(p, "GitHub reports") || strings.Contains(p, "genuine content conflict") {
		t.Errorf("the session is told only GitHub reported the conflict; prompt:\n%s", p)
	}
}

// pendingPastBudget is the watch's outcome in #421: CI on the pushed commit was
// still running when the poll budget ran out, on queued self-hosted runners.
func pendingPastBudget() ci.Outcome {
	return ci.Outcome{
		Pending: true,
		Reason:  "CI never reached a terminal state (still pending past the poll budget)",
		Failing: []ci.Check{{Name: "linting_and_tests", Bucket: ci.BucketPending}},
	}
}

// #421, replaying the incident: the auto-fix passed every local gate on the
// branch and the merge ref, was pushed, and CI was polled for 25 minutes without
// settling. The verdict is unknown, not red: the run log says CI is still
// pending, never "CI did not go green", and the pushed fix ships.
func TestFixNarratesCIPendingPastThePollBudgetApartFromARedCI(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "cifix-1" {
			repo.Commit(h.Worktree(), "thing.go", "package thing\n", "Fix TestThing")
		}
		return session.Outcome{}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
		return pendingPastBudget()
	}
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if fixErr != nil {
		t.Fatalf("the gate-green fix is accepted, got %v", fixErr)
	}
	if res.Disposition != stages.Shipped {
		t.Fatalf("a pushed fix ships whatever CI has yet to say, got %+v", res)
	}
	events := readEvents(t, log)
	if !strings.Contains(events, "CI still pending past the poll budget (not a red CI)") || !strings.Contains(events, "linting_and_tests") {
		t.Errorf("the run log must say CI is still pending, and on which check; events:\n%s", events)
	}
	if strings.Contains(events, "did not go green") {
		t.Errorf("CI still pending must not read as a red CI; events:\n%s", events)
	}
}

// #421: a watch that ran out of poll budget before it pushed anything launched no
// session and changed nothing on the branch. It is CIPending, not a failure to
// ship, so the Source takes the attempt back and the next poll re-checks that
// commit's CI before any fix session.
func TestFixThatPushedNothingBeforeCIStillPendingIsCIPending(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	tip := it.PR.HeadSHA
	h.CI = pendingPastBudget()
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if res.Disposition != stages.CIPending || res.OK {
		t.Fatalf("want CIPending, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("agents = %v, want none: the watch never reached a fix", agentLabels(h))
	}
	assertWatchPushedNothing(t, h, repo, it.PR.HeadBranch, tip)
	if events := readEvents(t, log); strings.Contains(events, "did not go green") {
		t.Errorf("CI still pending must not read as a red CI; events:\n%s", events)
	}
}

// pr1615 is herd's PR-1615 as pr-doctor polled it: Dependabot's production-deps
// group, a branch no harness ticket owns, so a conflict is merged, not rebased.
// Its lockfile clashes with main's, and main has moved on by 30 other files.
func pr1615(t *testing.T, k Kind) (Item, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	repo.Merge("web/pnpm-lock.yaml", "lockfileVersion: '9.0'\n", "add the lockfile")
	it := humanItem(k)
	it.PR.Number, it.PR.Title, it.PR.HeadBranch = 1615, "Bump the production-deps group", "dependabot/npm_and_yarn/web/production-deps-d6f8155"
	it.PR.HeadSHA = repo.PublishFile(it.PR.HeadBranch, "web/pnpm-lock.yaml", "lockfileVersion: '9.0'\nstripe: 17.0.0\n")
	main := map[string]string{"web/pnpm-lock.yaml": "lockfileVersion: '9.0'\nvite: 6.0.0\n"}
	for i := 0; i < 30; i++ {
		main[fmt.Sprintf("web/src/f%02d.ts", i)] = "export {}\n"
	}
	repo.MergeFiles(main, "main moved on")
	return it, repo
}

// #428, replaying herd PR-1615: attempt 2's conflict-resolution session merged
// origin/main into the Dependabot branch and stopped on the lockfile — MERGE_HEAD
// set, the lockfile unmerged, 30 files staged. Dependabot then rebased and
// force-pushed, and attempt 3 (a CI fix at the new head) could not drop the
// worktree, so the PR went to a human before any session. Now the run aborts the
// unfinished merge, saying so, and the next checkout starts at the new head.
func TestFixAbortsAnUnfinishedMergeSoTheNextCheckoutSucceeds(t *testing.T) {
	it, repo := pr1615(t, Conflict)
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			repo.UnfinishedMerge(h.Worktree(), "origin/main")
		}
		return session.Outcome{}
	}
	log := pipelineLog(t)

	if res := Fix(h, pipelineCfg(), log, it, prDoctor()); res.OK || res.Err != nil {
		t.Fatalf("an unresolved conflict ships nothing and needs no human, got %+v", res)
	}
	if events := readEvents(t, log); !strings.Contains(events, "aborted the unfinished merge in "+h.Worktree().Path()) {
		t.Errorf("the run must say it aborted the merge, naming the worktree:\n%s", events)
	}
	if tip := repo.Git(repo.Dir, "rev-parse", "refs/heads/"+it.PR.HeadBranch); tip != it.PR.HeadSHA {
		t.Errorf("the local branch is at %s, want the polled head %s: a half-resolved merge is no checkpoint", tip, it.PR.HeadSHA)
	}

	// Dependabot rebases and force-pushes; attempt 3 is a CI fix at the new head.
	rebased := repo.Republish(it.PR.HeadBranch, map[string]string{"web/pnpm-lock.yaml": "lockfileVersion: '9.0'\nvite: 6.0.0\nstripe: 17.0.0\n"})
	next := humanItem(CIRed)
	next.PR = it.PR
	next.PR.HeadSHA = rebased
	retry := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	retry.Branch = it.PR.HeadBranch

	res := Fix(retry, pipelineCfg(), pipelineLog(t), next, prDoctor())

	if res.Disposition == stages.CheckoutBlocked {
		t.Fatalf("the next attempt's checkout must succeed, got %+v", res)
	}
	if head, _ := retry.Worktree().HeadSHA(); retry.Worktree().Exists() && head != rebased {
		t.Errorf("the next attempt is at %s, want the force-pushed head %s", head, rebased)
	}
}

// #428: the same for a rebase a CI-fix session stopped on a conflict with main,
// which nothing on the CI-fix path aborts: the run aborts it, saying so, and saves
// no checkpoint, so the worktree is removed and the next checkout starts at
// origin's tip.
func TestFixAbortsAnUnfinishedRebase(t *testing.T) {
	h, it, repo := prHost(t, pr1384())
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "cifix-1" {
			wt := h.Worktree()
			repo.Commit(wt, "other.txt", "the session's change\n", "Fix the build")
			repo.UnfinishedRebase(wt, "origin/main")
		}
		return session.Outcome{ExitCode: 137, CapKilled: true}
	}
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		err := w.Fix(ci.FixInput{Logs: "FAIL web/src/routes/call/-components/more-tray.stories.tsx > MoreTray", LogAvailable: true})
		return ci.Outcome{Reason: fmt.Sprintf("auto-fix session failed: %v", err)}
	}
	log := pipelineLog(t)

	if res := Fix(h, pipelineCfg(), log, it, prDoctor()); res.OK || res.Err != nil {
		t.Fatalf("a cap-killed fix ships nothing and needs no human, got %+v", res)
	}
	if events := readEvents(t, log); !strings.Contains(events, "aborted the unfinished rebase in "+h.Worktree().Path()) {
		t.Errorf("the run must say it aborted the rebase, naming the worktree:\n%s", events)
	}
	if h.Worktree().Exists() {
		t.Error("the aborted worktree is clean, so it must be removed")
	}
	if subject := repo.Git(repo.Dir, "log", "-1", "--format=%s", "refs/heads/"+it.PR.HeadBranch); gitpkg.IsCheckpoint(subject) {
		t.Errorf("a stopped rebase is no checkpoint, but the local branch's tip is %q", subject)
	}
	if resumed, err := h.Worktree().CheckoutRemote(); err != nil || resumed != (gitpkg.Resumed{}) {
		t.Errorf("the next checkout = %+v, %v; want a plain checkout at origin's tip", resumed, err)
	}
}

// #428: when git refuses the abort (here its index is locked), the worktree is
// still mid-merge and the next checkout would refuse it, so the PR goes to a
// human at once, as for a checkpoint that cannot be committed (#246).
func TestFixWhoseUnfinishedMergeCannotBeAbortedNeedsAHuman(t *testing.T) {
	it, repo := pr1615(t, Conflict)
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(Key(it.PR.Number)))
	h.Branch = it.PR.HeadBranch
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "rebasefix" {
			wt := h.Worktree()
			repo.UnfinishedMerge(wt, "origin/main")
			lock := filepath.Join(repo.Git(wt.Path(), "rev-parse", "--absolute-git-dir"), "index.lock")
			if err := os.WriteFile(lock, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return session.Outcome{}
	}

	res := Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	var unsaved *UnsavedError
	if res.OK || !errors.As(res.Err, &unsaved) || unsaved.Path != h.Worktree().Path() {
		t.Fatalf("a refused abort needs a human, naming the worktree %s, got %+v", h.Worktree().Path(), res)
	}
	if !strings.Contains(res.Err.Error(), "merge --abort") {
		t.Errorf("the reason must carry the refused abort: %v", res.Err)
	}
	if !h.Worktree().Exists() {
		t.Error("the worktree must be kept for the human")
	}
}

// #467: after the auto-fix and a green CI, the retrospective runs in the run's
// worktree, which the pr-doctor cleanup removes only once the retrospective ends.
func TestFixRunsTheRetrospectiveInTheWorktreeBeforeTheCleanupRemovesIt(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		_ = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
		return ci.Outcome{OK: true, Reason: "green after the fix"}
	}
	var onDisk bool
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		switch run.Label {
		case "cifix-1":
			repo.Commit(h.Worktree(), "thing.go", "package thing\n", "Fix TestThing")
		case "retrospective":
			onDisk = h.Worktree().Exists() && run.WorkDir == h.Worktree().Path()
		}
		return session.Outcome{}
	}

	Fix(h, pipelineCfg(), pipelineLog(t), it, prDoctor())

	if !onDisk {
		t.Error("the retrospective must launch in the run's worktree, while it is still on disk")
	}
	if h.Worktree().Exists() {
		t.Error("the cleanup removes the clean worktree once the retrospective has ended")
	}
}

// #478, replaying the incident: a pushed conflict resolution, CI green, then a
// slow retrospective in the PR's worktree, about a minute into which a sweep that
// never reads the run lock (a Consumer's prune script, run outside the shield)
// removed the worktree. The run occupies the worktree, which outlives every sweep
// until the retrospective exits, and the run's own cleanup removes it after.
func TestFixOccupiesTheWorktreeUntilTheRetrospectiveExits(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(Conflict))
	path := h.Worktree().Path()
	sweep := func() error {
		return exec.Command("git", "-C", repo.Dir, "worktree", "remove", "--force", path).Run()
	}
	var swept, kept bool
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		switch run.Label {
		case "rebasefix":
			repo.Resolve(h.Worktree())
		case "retrospective":
			kept = true
			for i := 0; i < 5; i++ {
				swept = swept || sweep() == nil
				time.Sleep(10 * time.Millisecond)
				kept = kept && h.Worktree().Exists() && h.Worktree().Clean()
			}
		}
		return session.Outcome{}
	}
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a pushed, CI-green resolution ships, got %+v", res)
	}
	if swept || !kept {
		t.Error("a sweep removed the worktree while the retrospective was still running in it")
	}
	if h.Worktree().Exists() {
		t.Error("the run's cleanup removes the clean worktree once the retrospective has exited")
	}
	if events := readEvents(t, log); !strings.Contains(events, "removed the clean worktree") {
		t.Errorf("the cleanup must remove the worktree itself; events:\n%s", events)
	}
}

// #478: the occupancy lock a killed run left on the PR's worktree is lifted by the next run,
// which holds the run lock, so its checkout is not refused as local state.
func TestFixLiftsTheOccupancyLockAKilledRunLeftOnItsWorktree(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	if _, err := h.Worktree().CheckoutRemote(); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "worktree", "lock", "--reason", gitpkg.OccupiedReason, h.Worktree().Path())
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if res.Disposition == stages.CheckoutBlocked {
		t.Fatalf("a killed run's occupancy lock must not block the checkout: %+v", res)
	}
	if !strings.Contains(calls(h), "watch-ci|") {
		t.Errorf("the run must go on past the checkout; calls: %v", h.Calls)
	}
	if events := readEvents(t, log); !strings.Contains(events, "lifted the occupancy lock a killed run left") {
		t.Errorf("the lifted lock must be narrated; events:\n%s", events)
	}
}

// #467, replaying the incident: the worktree and its local branch went from under
// the run after the auto-fix and a green CI, at the hands of a remover that forces
// past the run's occupancy lock (#478), so the retrospective starts in the
// host checkout. Its prompt reads the change from origin/<branch>, which the
// checkout still has, and never names a `git -C` on the absent worktree.
func TestFixRetrospectiveOverAGoneWorktreeReadsTheRemoteTrackingRef(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		_ = w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})
		repo.Git(repo.Dir, "worktree", "remove", "--force", "--force", h.Worktree().Path())
		repo.Git(repo.Dir, "branch", "-D", it.PR.HeadBranch)
		return ci.Outcome{OK: true, Reason: "green after the fix"}
	}
	log := pipelineLog(t)

	Fix(h, pipelineCfg(), log, it, prDoctor())

	if got := agentLabels(h); len(got) != 2 || got[1] != "retrospective" {
		t.Fatalf("agents = %v, want the CIFix session then the retrospective", got)
	}
	p := h.Agents[1].Prompt
	for _, want := range []string{"git log origin/main..origin/feat/gh-7", "git diff origin/main...origin/feat/gh-7"} {
		if !strings.Contains(p, want) {
			t.Errorf("retrospective prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "git -C "+h.Worktree().Path()) {
		t.Errorf("retrospective prompt still reads the change in the absent worktree:\n%s", p)
	}
	if events := readEvents(t, log); !strings.Contains(events, "reads the change from origin/feat/gh-7") {
		t.Errorf("the run log must say the retrospective reads the remote-tracking ref; events:\n%s", events)
	}
}

// refusedChecksPoll is the watch's outcome in #466: GitHub answered the checks
// poll, and its one retry, with HTTP 401, so the watch read no CI state.
func refusedChecksPoll() ci.Outcome {
	return ci.Outcome{
		AuthFailed: true,
		Reason:     "CI watch read no CI state: gh not authenticated for the checks poll (HTTP 401 / bad credentials) — fix the harness gh auth; the PR is worked again on the next poll",
	}
}

// #466, replaying the incident: pr-doctor opened on a red PR and the checks poll
// was refused with a 401. The run logged a ✓, removed the worktree and launched
// no fix, so the red PR read as done. Now the run is CIAuthFailed, not OK, logs a
// failure mark with that outcome, launches nothing, pushes nothing and keeps the
// worktree for the next run.
func TestFixWhoseChecksPollGitHubRefusedIsCIAuthFailed(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	tip := it.PR.HeadSHA
	h.CI = refusedChecksPoll()
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if res.Disposition != stages.CIAuthFailed || res.OK {
		t.Fatalf("want CIAuthFailed and not OK, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("agents = %v, want none: the watch never read a red", agentLabels(h))
	}
	assertWatchPushedNothing(t, h, repo, it.PR.HeadBranch, tip)
	if !h.Worktree().Exists() {
		t.Error("a run that read no CI state keeps its worktree for the next run")
	}
	events := readEvents(t, log)
	if !strings.Contains(events, "pr-fix ✗ ci-watch-auth-failed: CI watch read no CI state") {
		t.Errorf("the run log must mark the refused poll as a failure; events:\n%s", events)
	}
	if strings.Contains(events, "pr-fix ✓") || strings.Contains(events, "removed the clean worktree") {
		t.Errorf("a refused poll must not read as a finished run; events:\n%s", events)
	}
}

// #466: a watch that pushed a fix before GitHub refused its next poll changed the
// branch on origin, so it ships as any pushed fix does, and its worktree goes.
func TestFixThatPushedBeforeTheChecksPollWasRefusedShips(t *testing.T) {
	h, it, repo := prHost(t, harnessItem(CIRed))
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "cifix-1" {
			repo.Commit(h.Worktree(), "thing.go", "package thing\n", "Fix TestThing")
		}
		return session.Outcome{}
	}
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		if err := w.Fix(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true}); err != nil {
			t.Fatalf("the gate-green fix is accepted, got %v", err)
		}
		return refusedChecksPoll()
	}
	log := pipelineLog(t)

	res := Fix(h, pipelineCfg(), log, it, prDoctor())

	if res.Disposition != stages.Shipped || res.OK {
		t.Fatalf("a pushed fix ships, not OK while CI is unread, got %+v", res)
	}
	if !strings.Contains(readEvents(t, log), "pr-fix ✗ ci-watch-auth-failed") {
		t.Error("the refused poll is still marked as a failure")
	}
}
