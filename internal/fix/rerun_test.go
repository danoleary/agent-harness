package fix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// storybookCrash is the storybook gate's log from herd PR #1312 (#167): 67 of 70
// tests passed, and the test browser's connection closed under the 68th.
const storybookCrash = ` Test Files  1 failed | 69 passed (70)
      Tests  67 passed (70)

⎯⎯⎯⎯⎯⎯ Unhandled Errors ⎯⎯⎯⎯⎯⎯
Error: Failed to run the test /work/herd/web/src/routes/call/-components/more-tray.stories.tsx.
Caused by: Error: [vitest] Browser connection was closed while running tests. Was the page closed unexpectedly?
Caused by: Error: [birpc] rpc is closed, cannot call "createTesters"
 ELIFECYCLE  Command failed with exit code 1.
`

// gateScript is one scripted gate run: the log it writes and the outcome it ends in.
type gateScript struct {
	log string
	out session.Outcome
}

// scriptedGates is an Env over a real worktree carrying a code change, whose gate
// runs play script in order (the last entry repeats), each writing its log where
// the Runner tees a gate's output. It returns the Env and the gate labels run.
func scriptedGates(t *testing.T, gates []config.Gate, script ...gateScript) (Env, *hostio.Fake) {
	t.Helper()
	repo := gittest.New(t)
	repo.Handoff("proj-1")
	h := hostio.NewFake(repo.Checkout(), "proj-1")
	log, err := runlog.New(t.TempDir(), "PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	h.ShellFn = func(call int, s hostio.ShellRun) session.Outcome {
		step := script[len(script)-1]
		if call <= len(script) {
			step = script[call-1]
		}
		if err := os.WriteFile(filepath.Join(log.Dir, s.Label+".log"), []byte(step.log), 0o644); err != nil {
			t.Fatal(err)
		}
		return step.out
	}
	cfg := config.Config{}
	cfg.Gates = gates
	cfg.GateLockDir = t.TempDir()
	return Env{Host: h, Cfg: cfg, Log: log, Who: "pr-fix"}, h
}

func shellLabels(h *hostio.Fake) string {
	var out []string
	for _, s := range h.Shells {
		out = append(out, s.Label)
	}
	return strings.Join(out, ",")
}

var storybook = []config.Gate{{Name: "storybook", Command: "pnpm run test-storybook:changed"}}

// The PR-1312 incident (#167): the test browser crashed under one story the fix
// never touched. That is the environment, not the fix, so the gate is re-run once,
// and the re-run's green is the gate's verdict.
func TestAGateThatCrashedTheTestBrowserIsRerunOnce(t *testing.T) {
	e, h := scriptedGates(t, storybook,
		gateScript{log: storybookCrash, out: session.Outcome{ExitCode: 1}},
		gateScript{log: " Tests  70 passed (70)\n"},
	)

	res := e.Gates("gate-cifix1")

	if !res.Green() {
		t.Fatalf("a crash the re-run washes out is green, got %+v", res)
	}
	if got := shellLabels(h); got != "gate-cifix1-storybook,gate-cifix1-storybook-rerun" {
		t.Errorf("gate runs = %s, want the gate then one re-run", got)
	}
	raw, _ := os.ReadFile(filepath.Join(e.Log.Dir, "run.jsonl"))
	if !strings.Contains(string(raw), "re-running it once") {
		t.Errorf("the re-run must be narrated:\n%s", raw)
	}
}

// Each infrastructure signature earns the one re-run; a plain red does not.
func TestAGateIsRerunOnlyOverAnInfrastructureSignature(t *testing.T) {
	cases := []struct {
		name  string
		red   gateScript
		rerun bool
	}{
		{"test browser closed", gateScript{log: storybookCrash, out: session.Outcome{ExitCode: 1}}, true},
		{"node out of memory", gateScript{log: "FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory\n", out: session.Outcome{ExitCode: 134}}, true},
		{"a child SIGKILLed", gateScript{log: "make: *** [test] Killed\nKilled\n", out: session.Outcome{ExitCode: 2}}, true},
		{"corepack download", gateScript{log: "Internal Error: Error when performing the request to https://registry.npmjs.org/pnpm/-/pnpm-9.1.0.tgz\n", out: session.Outcome{ExitCode: 1}}, true},
		{"network reset", gateScript{log: "npm ERR! code ECONNRESET\n", out: session.Outcome{ExitCode: 1}}, true},
		{"gate cap", gateScript{log: "running…\n", out: session.Outcome{ExitCode: sandbox.ExitOOMKill, CapKilled: true}}, true},
		{"stderr only", gateScript{out: session.Outcome{ExitCode: 1, StderrTail: "Error: [vitest] Browser connection was closed while running tests."}}, true},
		{"a real red", gateScript{log: "--- FAIL: TestThing (0.00s)\n    thing_test.go:12: got 1, want 2\nFAIL\n", out: session.Outcome{ExitCode: 1}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, h := scriptedGates(t, storybook, c.red)

			res := e.Gates("gate")

			if res.Green() {
				t.Fatalf("a gate red on every run stays red, got %+v", res)
			}
			if got, want := len(h.Shells), map[bool]int{true: 2, false: 1}[c.rerun]; got != want {
				t.Errorf("gate runs = %s, want %d", shellLabels(h), want)
			}
		})
	}
}

// A re-run that is red again is the verdict: the gate is red, and its log is the
// re-run's, which is what a later session is handed.
func TestARerunGateThatIsRedAgainIsRed(t *testing.T) {
	e, h := scriptedGates(t, append(storybook, config.Gate{Name: "after", Command: "true"}),
		gateScript{log: storybookCrash, out: session.Outcome{ExitCode: 1}},
	)

	res := e.Gates("gate")

	if res.Green() || res.FailedGate != "storybook" {
		t.Fatalf("got %+v, want storybook red", res)
	}
	if res.Log != "gate-storybook-rerun.log" {
		t.Errorf("Log = %q, want the re-run's log", res.Log)
	}
	if got := shellLabels(h); got != "gate-storybook,gate-storybook-rerun" {
		t.Errorf("gate runs = %s: one re-run, and nothing after a red", got)
	}
}

// Over a fix, a red whose failures name only files the fix did not touch is re-run
// once too; a failure in a file the fix changed is the fix's, and is final.
func TestAFixGateRedOnlyInUntouchedFilesIsRerunOnce(t *testing.T) {
	red := gateScript{log: "FAIL src/widgets/other.test.ts > renders\nError: expected 1 to be 2\n", out: session.Outcome{ExitCode: 1}}
	cases := []struct {
		name  string
		fixed []string
		rerun bool
	}{
		{"untouched", []string{"src/build/config.ts"}, true},
		{"a fix that changed nothing", []string{}, true},
		{"touched", []string{"src/widgets/other.test.ts"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, h := scriptedGates(t, []config.Gate{{Name: "test", Command: "pnpm test"}}, red)

			e.GatesOver("gate-cifix1", c.fixed)

			if got, want := len(h.Shells), map[bool]int{true: 2, false: 1}[c.rerun]; got != want {
				t.Errorf("gate runs = %s, want %d", shellLabels(h), want)
			}
		})
	}
	// Plain Gates knows no fix, so the same red is final there.
	e, h := scriptedGates(t, []config.Gate{{Name: "test", Command: "pnpm test"}}, red)
	e.Gates("gate")
	if len(h.Shells) != 1 {
		t.Errorf("gate runs = %s, want no re-run without a fix to compare against", shellLabels(h))
	}
}
