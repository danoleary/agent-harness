package fix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// storybookGate is herd's Storybook gate: it runs only the stories the diff
// against origin/main reaches.
var storybookGate = config.Gate{Name: "test-storybook", Command: "cd web && pnpm storybook-gate --changed origin/main"}

// storyRedOnBranch is #448's shell: the post-resolution Storybook gate is red on
// one story, and the same command over origin/main alone, where the diff is
// empty, selects nothing and exits 0.
func storyRedOnBranch(e *Env) func(hostio.ShellRun) session.Outcome {
	return func(run hostio.ShellRun) session.Outcome {
		if !strings.Contains(run.Command, "storybook-gate") {
			return session.Outcome{}
		}
		if strings.HasPrefix(run.Label, "basegate") {
			if strings.Contains(run.Command, "--changed origin/main") {
				writeGateLog(e, run, "no story reaches the diff; 0 stories ran; exit 0\n")
				return session.Outcome{}
			}
			return session.Outcome{ExitCode: 1, StderrTail: "FAIL web/src/Board.stories.tsx"}
		}
		return session.Outcome{ExitCode: 1, StderrTail: "FAIL web/src/Board.stories.tsx"}
	}
}

func writeGateLog(e *Env, run hostio.ShellRun, text string) {
	_ = os.WriteFile(filepath.Join(e.Log.Dir, run.Label+".log"), []byte(text), 0o644)
}

func baseRuns(h *hostio.Fake) []hostio.ShellRun {
	var out []hostio.ShellRun
	for _, s := range h.Shells {
		if strings.HasPrefix(s.Label, "basegate") {
			out = append(out, s)
		}
	}
	return out
}

// #448, replayed: the red re-gate's gate is scoped by the diff against
// origin/main. Over origin/main alone that diff is empty, so the same command
// selects nothing and passes. Before, the base was gated anyway (a prep and a gate
// slot) and declared green. Now the check is inconclusive, says so, runs nothing
// on the base and records no verdict for it.
func TestARedDiffScopedGateIsNotComparedOnTheBaseWithoutABaseCommand(t *testing.T) {
	var e Env
	e, h := redBaseEnv(t, func(run hostio.ShellRun) session.Outcome { return storyRedOnBranch(&e)(run) })
	e.Cfg.Gates = []config.Gate{storybookGate}
	base, _ := h.Worktree().RefSHA("origin/main")

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

	if res.OK || res.BaseRed != nil || !res.Resolved {
		t.Fatalf("want the branch's own red re-gate, got %+v", res)
	}
	if runs := baseRuns(h); len(runs) != 0 {
		t.Errorf("gated origin/main with a diff-scoped gate that selects nothing there: %+v", runs)
	}
	if v, ok := basegate.For(e.Cfg).Read(base); ok {
		t.Errorf("recorded verdict %+v for a base no gate tested", v)
	}
	if got := runLog(t, e); !strings.Contains(got, "inconclusive: 0 targets selected") {
		t.Errorf("run.jsonl must call the base check inconclusive; got %s", got)
	}
}

// A diff-scoped gate with a base_command is compared on the base over the
// branch's targets: {paths} carries the paths the branch's diff against
// origin/main touches, the input its own run selected from. The failing story
// fails on origin/main too, so the base is red, not the branch.
func TestARedDiffScopedGateRunsItsBaseCommandOverTheBranchsPaths(t *testing.T) {
	var e Env
	e, h := redBaseEnv(t, func(run hostio.ShellRun) session.Outcome { return storyRedOnBranch(&e)(run) })
	g := storybookGate
	g.BaseCommand = "cd web && pnpm storybook-gate --files {paths}"
	e.Cfg.Gates = []config.Gate{{Name: "build", Command: "make build"}, g}
	paths, err := h.Worktree().DiffPaths()
	if err != nil || len(paths) == 0 {
		t.Fatalf("the resolved branch must differ from origin/main: %v %v", paths, err)
	}

	res := e.ResolveConflict(Conflict{Regate: func() GateResult {
		return GateResult{Outcome: session.Outcome{ExitCode: 1}, FailedGate: "test-storybook"}
	}})

	if res.BaseRed == nil || res.BaseRed.Verdict.Gate != "test-storybook" {
		t.Fatalf("the story fails on origin/main too, so the base is red; got %+v", res)
	}
	runs := baseRuns(h)
	if len(runs) == 0 {
		t.Fatal("origin/main was not gated")
	}
	var story string
	for _, r := range runs {
		if r.Label == "basegate-build" && r.Command != "make build" {
			t.Errorf("a gate the branch did not fail runs as configured on the base, got %q", r.Command)
		}
		if r.Label == "basegate-test-storybook" {
			story = r.Command
		}
	}
	want := "cd web && pnpm storybook-gate --files " + shellQuote(paths)
	if story != want {
		t.Errorf("base storybook command = %q, want %q", story, want)
	}
}

// A green verdict recorded for the base (the ticket loop gated it untouched)
// says nothing of a diff-scoped gate: it selected nothing there. A gate with a
// base_command is compared on the base all the same.
func TestARecordedGreenBaseDoesNotAnswerForADiffScopedGate(t *testing.T) {
	var e Env
	e, h := redBaseEnv(t, func(run hostio.ShellRun) session.Outcome { return storyRedOnBranch(&e)(run) })
	g := storybookGate
	g.BaseCommand = "pnpm storybook-gate --files {paths}"
	e.Cfg.Gates = []config.Gate{g}
	base, _ := h.Worktree().RefSHA("origin/main")
	if _, err := basegate.For(e.Cfg).Write(base, basegate.Verdict{}); err != nil {
		t.Fatal(err)
	}

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

	if res.BaseRed == nil || !res.BaseRed.Recorded {
		t.Fatalf("want the base gated over the branch's targets and found red, got %+v", res)
	}
}

// The backstop: a gate whose command does not name origin/main (a wrapper such as
// `make test-changed`) can still be diff-scoped. A base run of the branch's red
// gate that reports it ran zero tests is inconclusive, never a green base.
func TestABaseRunThatRanZeroTestsIsInconclusive(t *testing.T) {
	var e Env
	e, _ = redBaseEnv(t, func(run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, "basegate") {
			writeGateLog(&e, run, "no test reaches the diff; 0 tests ran; exit 0\n")
			return session.Outcome{}
		}
		return mainDoesNotBuild(run)
	})
	e.Cfg.Gates = []config.Gate{{Name: "check", Command: "make test-changed"}}

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

	if res.BaseRed != nil || res.OK {
		t.Fatalf("an inconclusive base leaves the red re-gate the branch's, got %+v", res)
	}
	if got := runLog(t, e); !strings.Contains(got, "inconclusive: gate") || !strings.Contains(got, "ran no tests on origin/main") {
		t.Errorf("run.jsonl must call the base check inconclusive; got %s", got)
	}
}

func TestSelectedNothing(t *testing.T) {
	for log, want := range map[string]bool{
		"no story reaches the diff; 0 stories ran; exit 0": true,
		"no test reaches the diff; 0 tests ran; exit 0":    true,
		"Ran 0 tests in 0.000s\n\nOK":                      true,
		"Test Files  0 passed (0)\n0 tests executed":       true,
		"10 tests ran, 0 failed":                           false,
		"120 stories ran":                                  false,
		"ok  \tpkg\t0.2s\n?   \tother\t[no test files]":    false,
		"no tests failed":                                  false,
		"":                                                 false,
	} {
		if got := selectedNothing(log); got != want {
			t.Errorf("selectedNothing(%q) = %v, want %v", log, got, want)
		}
	}
}
