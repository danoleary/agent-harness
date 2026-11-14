package fix

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// selfGates is this repo's own gate list (#221): `check` is skipped for a diff
// that touches only .github/**, `build` always runs.
var selfGates = []config.Gate{
	{Name: "check", Command: "make check", SkipWhenOnly: []string{".github/**"}},
	{Name: "build", Command: "make build"},
}

// pinBump is the PR-217 resolution (#221): one line of a workflow, two adjacent
// action-pin bumps.
const pinBump = `jobs:
  check:
    steps:
      - uses: actions/checkout@08eba0b27e820071cde6df949e0beb9ba4906955 # v4.3.0
      - uses: actions/setup-go@d35c59abb061a4a6fb18e82ac0862c26744d6ab5 # v5.5.0
`

// skipEnv is an Env over a worktree whose only change against origin/main is the
// files given, with every gate run recorded and green.
func skipEnv(t *testing.T, honour bool, files ...string) (Env, *hostio.Fake) {
	t.Helper()
	repo := gittest.New(t)
	wt := repo.Worktree("pr-217")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		repo.Commit(wt, f, pinBump, "Bump the action pins")
	}
	h := hostio.NewFake(repo.Checkout(), "pr-217")
	h.ShellFn = func(int, hostio.ShellRun) session.Outcome { return session.Outcome{} }
	cfg := config.Config{}
	cfg.Gates = selfGates
	cfg.GateLockDir = t.TempDir()
	return Env{Host: h, Cfg: cfg, Log: testLog(t), Who: "pr-fix", SkipWhenOnly: honour}, h
}

// PR-217, replayed (#221): a pr-doctor fix whose diff is one workflow file skips
// `check` host-side, says why, and still runs `build`, which has no list.
func TestAPRFixTouchingOnlyAWorkflowSkipsTheGateThatListsIt(t *testing.T) {
	e, h := skipEnv(t, true, ".github/workflows/ci.yml")

	if res := e.Gates("gate"); !res.Green() {
		t.Fatalf("gates = %+v, want green", res)
	}
	if got := shellLabels(h); got != "gate-build" {
		t.Errorf("gates run = %q, want only gate-build", got)
	}
	got := runLog(t, e)
	for _, want := range []string{`gate \"check\" skipped`, ".github/**", "CI"} {
		if !strings.Contains(got, want) {
			t.Errorf("run.jsonl must say %q; got %s", want, got)
		}
	}
}

// A diff with one path outside the list runs every gate.
func TestAPRFixTouchingAPathOutsideTheListRunsTheGate(t *testing.T) {
	e, h := skipEnv(t, true, ".github/workflows/ci.yml", "internal/fix/fix.go")

	e.Gates("gate")
	if got := shellLabels(h); got != "gate-check,gate-build" {
		t.Errorf("gates run = %q, want both", got)
	}
}

// The implementation and review stages ignore the key: an Env that does not
// honour it runs the gate over the same workflow-only diff.
func TestAnEnvThatDoesNotHonourTheListRunsEveryGate(t *testing.T) {
	e, h := skipEnv(t, false, ".github/workflows/ci.yml")

	e.Gates("gate")
	if got := shellLabels(h); got != "gate-check,gate-build" {
		t.Errorf("gates run = %q, want both", got)
	}
}

// A branch with no diff against origin/main skips nothing: there is no fix whose
// paths could match.
func TestAnEmptyDiffSkipsNoGate(t *testing.T) {
	e, h := skipEnv(t, true)

	e.Gates("gate")
	if got := shellLabels(h); got != "gate-check,gate-build" {
		t.Errorf("gates run = %q, want both", got)
	}
}

// Every gate skipped is a green result with no gate run.
func TestAPRFixWhoseEveryGateIsSkippedIsGreen(t *testing.T) {
	e, h := skipEnv(t, true, ".github/dependabot.yml")
	e.Cfg.Gates = selfGates[:1]

	if res := e.Gates("gate"); !res.Green() {
		t.Fatalf("gates = %+v, want green", res)
	}
	if len(h.Shells) != 0 {
		t.Errorf("gates run = %q, want none", shellLabels(h))
	}
}
