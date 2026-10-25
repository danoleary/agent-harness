package prloop

import (
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/stages"
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

// fakeHost is a healthy Fake whose worktree HEAD is the polled PR head.
func fakeHost() *hostio.Fake {
	h := hostio.NewFake()
	h.Head = "abc1234"
	return h
}

func harnessItem(k Kind) Item {
	return Item{PR: github.PullRequest{Number: 12, Title: "#7: a ticket", HeadBranch: "feat/gh-7", HeadSHA: "abc1234", BaseBranch: "main"}, Kind: k, Harness: true}
}

func humanItem(k Kind) Item {
	return Item{PR: github.PullRequest{Number: 12, Title: "Widget", HeadBranch: "alice/widget", HeadSHA: "abc1234", BaseBranch: "main"}, Kind: k}
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
	h := fakeHost()

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), prDoctor())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a resolved, gate-green, CI-green conflict ships, got %+v", res)
	}
	got := calls(h)
	for _, want := range []string{"checkout-branch pr-12|", "shell prep|", "agent rebasefix|", "shell gate-check|", "push-force pr-12|", "watch-ci pr-12|", "agent retrospective|"} {
		if !strings.Contains(got, want) {
			t.Errorf("calls missing %q:\n%v", want, h.Calls)
		}
	}
	if strings.Index(got, "shell gate-check") > strings.Index(got, "push-force") {
		t.Error("the gates must run before the push")
	}
	if strings.Contains(got, "|push pr-12|") || strings.Contains(got, "create-pr") {
		t.Errorf("a rebased branch is force-pushed to the existing PR, never pushed plain or re-opened: %v", h.Calls)
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
	h := fakeHost()

	res := Fix(h, pipelineCfg(), pipelineLog(t), humanItem(Conflict), prDoctor())

	if !res.OK {
		t.Fatalf("a resolved, gate-green merge ships, got %+v", res)
	}
	got := calls(h)
	if !strings.Contains(got, "|push pr-12|") || strings.Contains(got, "push-force") {
		t.Errorf("a human branch is pushed without force; calls = %v", h.Calls)
	}
	if !strings.Contains(h.Agents[0].Prompt, "git merge --no-ff origin/main") || !strings.Contains(h.Agents[0].Prompt, "`alice/widget`") {
		t.Errorf("the session must merge main into alice/widget; prompt:\n%s", h.Agents[0].Prompt)
	}
	if !h.Watches[0].KeepHistory {
		t.Error("the CI watch must not rebase a human branch")
	}
}

// Red CI: the watch re-runs the failed checks, then hands the failing logs to a
// CIFix session; the harness re-runs the gates over the committed fix before the
// watch may push it.
func TestFixRunsTheCIFixSessionAndGatesItsCommit(t *testing.T) {
	h := fakeHost()
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL TestThing", true)
		return ci.Outcome{OK: true, Reason: "green after the fix"}
	}

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(CIRed), prDoctor())

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
	if !strings.Contains(calls(h), "agent cifix-1|ci-rerun-commit abc1234|shell gate-cifix1-check|") {
		t.Errorf("the gates must run over the committed fix; calls = %v", h.Calls)
	}
}

// A flake the watch's re-run washed out needs no session at all — and with no
// transcript there is nothing for a retrospective to mine.
func TestFixLaunchesNothingWhenTheRerunGoesGreen(t *testing.T) {
	h := fakeHost()

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(CIRed), prDoctor())

	if !res.OK || len(h.Agents) != 0 {
		t.Fatalf("a green re-run ships with no session, got %+v agents %v", res, agentLabels(h))
	}
}

// Ground truth, never the agent's word: a resolution that left the branch off its
// base pushes nothing.
func TestFixPushesNothingWhenTheResolutionFailsGroundTruth(t *testing.T) {
	h := fakeHost()
	h.Rebased = false

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), prDoctor())

	if res.OK || res.Disposition != stages.NoPR {
		t.Fatalf("an unverified resolution is a no-PR run, got %+v", res)
	}
	assertNothingPushed(t, h)
}

// A red gate over the resolved tree pushes nothing.
func TestFixPushesNothingWhenTheGatesFail(t *testing.T) {
	for _, it := range []Item{harnessItem(Conflict), humanItem(Conflict)} {
		h := fakeHost()
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
		assertNothingPushed(t, h)
	}
}

// A red gate over a CIFix commit refuses the fix, so the watch pushes nothing.
func TestFixRefusesACIFixWhoseGatesFail(t *testing.T) {
	h := fakeHost()
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

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(CIRed), prDoctor())

	if fixErr == nil || !strings.Contains(fixErr.Error(), `gate "check"`) {
		t.Fatalf("a fix over a red gate must be refused naming the gate, got %v", fixErr)
	}
	if res.OK || res.Disposition != stages.NoPR {
		t.Errorf("a refused fix pushed nothing, so the run is no-PR, got %+v", res)
	}
}

// A push that landed between the poll and the checkout is someone else's newer
// head: the loop works what it polled or nothing.
func TestFixWorksOnlyThePolledHead(t *testing.T) {
	h := fakeHost()
	h.Head = "fff9999"

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), prDoctor())

	if res.OK || len(h.Agents) != 0 {
		t.Fatalf("a moved head is not worked, got %+v agents %v", res, agentLabels(h))
	}
	assertNothingPushed(t, h)
}

// A spending cap that killed the session before it did anything is the daemon's
// back-off signal, not a failure to ship.
func TestFixSurfacesASpendingCap(t *testing.T) {
	h := fakeHost()
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		return session.Outcome{ExitCode: 1, SpendingCapAbort: true}
	}

	if res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), prDoctor()); res.Disposition != stages.CapAborted {
		t.Fatalf("a cap-aborted resolution is CapAborted, got %+v", res)
	}
}

// A host that cannot launch a sandbox is environmental: nothing is checked out.
func TestFixStopsAtAFailedPreflight(t *testing.T) {
	h := fakeHost()
	h.PreflightErr = errFake

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), prDoctor())

	if res.Disposition != stages.PreflightAborted || strings.Contains(calls(h), "checkout-branch") {
		t.Fatalf("a failed preflight aborts before the checkout, got %+v calls %v", res, h.Calls)
	}
}

// A loop without watch_ci stops at the push: the resolved branch ships, and CI is
// left to report on its own.
func TestFixWithoutWatchCIStopsAtThePush(t *testing.T) {
	h := fakeHost()

	res := Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), withoutSteps(config.StepWatchCI))

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a pushed resolution ships, got %+v", res)
	}
	got := calls(h)
	if !strings.Contains(got, "push-force pr-12|") || strings.Contains(got, "watch-ci") {
		t.Errorf("want a push and no CI watch; calls = %v", h.Calls)
	}
}

// A loop without retro files no findings: no retrospective session runs.
func TestFixWithoutRetroRunsNoRetrospective(t *testing.T) {
	h := fakeHost()

	Fix(h, pipelineCfg(), pipelineLog(t), harnessItem(Conflict), withoutSteps(config.StepRetro))

	if got := agentLabels(h); strings.Contains(strings.Join(got, ","), "retrospective") {
		t.Errorf("agents = %v, want no retrospective", got)
	}
}

// A fix step's prompt body is the Consumer's half of the RebaseFix and CIFix
// prompts, inside the same envelope.
func TestFixCarriesTheFixStepsPromptBody(t *testing.T) {
	h := fakeHost()
	cfg := pipelineCfg()
	cfg.Prompts.Fix = "Run `make lint` before you commit."

	Fix(h, cfg, pipelineLog(t), harnessItem(Conflict), prDoctor())

	p := h.Agents[0].Prompt
	if !strings.Contains(p, "Run `make lint` before you commit.") || !strings.Contains(p, "git reset --hard origin/main") {
		t.Errorf("the RebaseFix prompt must carry the body inside its envelope:\n%s", p)
	}
}

func assertNothingPushed(t *testing.T, h *hostio.Fake) {
	t.Helper()
	got := calls(h)
	if strings.Contains(got, "push pr-12") || strings.Contains(got, "push-force") || strings.Contains(got, "watch-ci") {
		t.Errorf("nothing may be pushed or watched; calls = %v", h.Calls)
	}
}

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

const errFake = fakeErr("docker unreachable")
