package fix

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// BEH-571: sessionError maps a finished auto-fix session's outcome to the
// error its ci.Driver.Fix callback returns. A spending-cap abort is its own
// retry-after-reset class (ci.ErrSpendingCapActive), distinct from a generic
// non-zero exit, and takes precedence over the exit code (a cap abort also exits
// non-zero) so a cap-active window isn't mislabelled as a fix-attempt failure.
func TestFixSessionErrorSpendingCapTakesPrecedence(t *testing.T) {
	// Cap aborts also carry a non-zero exit; the cap class must win.
	err := sessionError(session.Outcome{ExitCode: 1, SpendingCapAbort: true}, 1)
	if !errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("err = %v, want ci.ErrSpendingCapActive", err)
	}
}

func TestFixSessionErrorNonZeroExitIsGenericFailure(t *testing.T) {
	err := sessionError(session.Outcome{ExitCode: 2}, 3)
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("a plain non-zero exit must not be the spending-cap class: %v", err)
	}
	if !strings.Contains(err.Error(), "exited 2") || !strings.Contains(err.Error(), "session 3") {
		t.Fatalf("err %q should name the attempt and exit code", err)
	}
}

func TestFixSessionErrorCleanSessionReturnsNil(t *testing.T) {
	if err := sessionError(session.Outcome{ExitCode: 0}, 1); err != nil {
		t.Fatalf("a clean session must return nil, got %v", err)
	}
}

// BEH-634: RunGates iterates the config-declared named gate list host-side,
// running each gate in order and stopping at the first non-green one. When every
// gate passes it runs them all and reports no failing gate; when one fails it
// reports that gate's name and does not run the gates after it (the push gate
// only needs the first red to withhold the push, and skipping the rest saves a
// container launch). The failing gate's name is what flows into the log + CI-fix
// diagnosis, matching how internal/ci names a failing check.
func TestRunGatesAllGreenRunsEveryGateInOrder(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	var ran []string
	res := RunGates(gates, func(g config.Gate) session.Outcome {
		ran = append(ran, g.Name)
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "" {
		t.Errorf("all-green gates must report no failing gate, got %q", res.FailedGate)
	}
	if res.Outcome.ExitCode != 0 {
		t.Errorf("all-green outcome must be exit 0, got %d", res.Outcome.ExitCode)
	}
	if !reflect.DeepEqual(ran, []string{"check", "typecheck"}) {
		t.Errorf("gates must run in config order, ran %v", ran)
	}
}

func TestRunGatesStopsAtFirstFailureAndReportsItsName(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	var ran []string
	res := RunGates(gates, func(g config.Gate) session.Outcome {
		ran = append(ran, g.Name)
		if g.Name == "check" {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "check" {
		t.Errorf("the first red gate must be reported by name, got %q", res.FailedGate)
	}
	if res.Outcome.ExitCode != 2 {
		t.Errorf("the failing gate's outcome (exit 2) must be returned, got %d", res.Outcome.ExitCode)
	}
	if !reflect.DeepEqual(ran, []string{"check"}) {
		t.Errorf("gates after the first failure must not run, ran %v", ran)
	}
}

func TestRunGatesReportsLaterFailingGateByName(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	res := RunGates(gates, func(g config.Gate) session.Outcome {
		if g.Name == "typecheck" {
			return session.Outcome{ExitCode: 1}
		}
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "typecheck" {
		t.Errorf("the failing gate (typecheck) must be reported by name, got %q", res.FailedGate)
	}
}

// #248: a CI-fix session never drives a browser, so it starts without the
// browser-verification MCP servers a Consumer's project config declares. In the
// incident the Playwright and Chrome DevTools servers idled in the sandbox for
// the whole session.
func TestTheCIFixSessionStartsWithoutMCPServers(t *testing.T) {
	e, _ := conflictEnv(t, func(*gittest.Repo, *hostio.Fake) {})

	_ = e.CI(Target{})(ci.FixInput{Logs: "FAIL src/Swatch.stories.tsx", LogAvailable: true})

	h := e.Host.(*hostio.Fake)
	if len(h.Agents) != 1 {
		t.Fatalf("want one CI-fix session, got %d", len(h.Agents))
	}
	if !h.Agents[0].NoMCP {
		t.Error("the CI-fix session must start with no MCP servers")
	}
}

// #245: a fix session starts in the worktree it fixes, so the skills and settings
// it loads are the branch's, not whatever the host checkout has checked out.
func TestFixSessionsStartInTheWorktree(t *testing.T) {
	e, _ := conflictEnv(t, func(*gittest.Repo, *hostio.Fake) {})
	_ = e.CI(Target{})(ci.FixInput{Logs: "FAIL src/Swatch.stories.tsx", LogAvailable: true})
	_ = e.ResolveConflict(Conflict{Regate: func() GateResult { return GateResult{} }})

	h := e.Host.(*hostio.Fake)
	if len(h.Agents) != 2 {
		t.Fatalf("want a CI-fix and a conflict-resolution session, got %d", len(h.Agents))
	}
	// #391: both hand off a commit, so neither is told it is read-only.
	if got := []sandbox.Role{h.Agents[0].Role, h.Agents[1].Role}; got[0] != sandbox.RoleCIFix || got[1] != sandbox.RoleRebaseFix {
		t.Errorf("session roles = %v, want [%s %s]", got, sandbox.RoleCIFix, sandbox.RoleRebaseFix)
	}
	for _, run := range h.Agents {
		if want := h.Worktree().Path(); run.WorkDir != want {
			t.Errorf("%s session starts in %q, want the worktree %q", run.Label, run.WorkDir, want)
		}
	}
}

// gatedCIEnv is conflictEnv with one gate, so a gate run shows in the Host's calls.
func gatedCIEnv(t *testing.T, session func(*gittest.Repo, *hostio.Fake)) (Env, *gittest.Repo) {
	t.Helper()
	e, repo := conflictEnv(t, session)
	e.Cfg.Gates = []config.Gate{{Name: "check", Command: "make check"}}
	return e, repo
}

// gateRuns is the labels of the gate runs the Host made.
func gateRuns(h *hostio.Fake) []string {
	var labels []string
	for _, s := range h.Shells {
		if strings.HasPrefix(s.Label, "gate") {
			labels = append(labels, s.Label)
		}
	}
	return labels
}

// #396: the CIFix session could not reproduce the red and committed nothing, and
// the harness added its empty re-trigger commit. The tree is the one the gates
// already passed before the push, and origin/main has not moved past the branch's
// base, so there is nothing new to gate: the re-trigger is accepted with no gate.
func TestACIFixThatCommittedNothingRunsNoGate(t *testing.T) {
	e, _ := gatedCIEnv(t, func(*gittest.Repo, *hostio.Fake) {})
	h := e.Host.(*hostio.Fake)
	g := e.GatedCI(Target{Slug: "proj-1"})

	if err := g.Fix(ci.FixInput{Logs: "FAIL TestFlaky", LogAvailable: true}); err != nil {
		t.Fatalf("a re-trigger with no code change is accepted, got %v", err)
	}
	if runs := gateRuns(h); len(runs) != 0 {
		t.Errorf("gate runs = %v, want none over a fix that changed nothing", runs)
	}
	if g.Accepted != 1 {
		t.Errorf("Accepted = %d, want the re-trigger accepted for the push", g.Accepted)
	}
	if log := runLog(t, e); !strings.Contains(log, "no code change from auto-fix 1") {
		t.Errorf("the run log does not say why the re-gate was skipped:\n%s", log)
	}
}

// #396: origin/main moving past the branch's base is the one new input a fix that
// changed nothing brings, since CI checks the merge with it: the merge gate runs,
// the branch gate does not. A second such fix over the same origin/main has
// nothing new at all, so it runs no gate.
func TestACIFixThatCommittedNothingGatesOnlyTheMergeWithAMovedMain(t *testing.T) {
	e, repo := gatedCIEnv(t, func(*gittest.Repo, *hostio.Fake) {})
	h := e.Host.(*hostio.Fake)
	repo.Merge("other.txt", "main moved\n", "a sibling lands on main")
	g := e.GatedCI(Target{Slug: "proj-1"})

	if err := g.Fix(ci.FixInput{Logs: "FAIL TestFlaky", LogAvailable: true}); err != nil {
		t.Fatalf("a re-trigger green merged with main is accepted, got %v", err)
	}
	if runs := gateRuns(h); !reflect.DeepEqual(runs, []string{"gate-cifix1-merge-check"}) {
		t.Errorf("gate runs = %v, want only the merge gate", runs)
	}
	if err := g.Fix(ci.FixInput{Logs: "FAIL TestFlaky", LogAvailable: true}); err != nil {
		t.Fatalf("a second re-trigger is accepted, got %v", err)
	}
	if runs := gateRuns(h); len(runs) != 1 {
		t.Errorf("gate runs = %v, want none for a second re-trigger over the same origin/main", runs)
	}
}

// A CIFix session that commits a code change still has it gated, on the branch
// and merged with a moved origin/main.
func TestACIFixThatCommittedACodeChangeIsGated(t *testing.T) {
	e, repo := gatedCIEnv(t, func(repo *gittest.Repo, h *hostio.Fake) {
		repo.Commit(h.Worktree(), "fix.go", "package fix\n", "fix the flake")
	})
	h := e.Host.(*hostio.Fake)
	repo.Merge("other.txt", "main moved\n", "a sibling lands on main")

	if err := e.GatedCI(Target{Slug: "proj-1"}).Fix(ci.FixInput{Logs: "FAIL TestFlaky", LogAvailable: true}); err != nil {
		t.Fatalf("a green fix is accepted, got %v", err)
	}
	if runs := gateRuns(h); !reflect.DeepEqual(runs, []string{"gate-cifix1-check", "gate-cifix1-merge-check"}) {
		t.Errorf("gate runs = %v, want the branch gate then the merge gate", runs)
	}
}
