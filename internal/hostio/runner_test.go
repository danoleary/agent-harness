package hostio

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// launch is one recorded container launch: the argv the Runner built and the
// options it passed alongside.
type launch struct {
	args []string
	opts session.Options
}

// testRunner returns a Runner whose launches are recorded rather than run, with a
// deterministic pid and an instant sleep.
func testRunner(t *testing.T, outcomes ...session.Outcome) (*Runner, *[]launch) {
	t.Helper()
	log, err := runlog.New(t.TempDir(), "PROJ-1")
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	var seen []launch
	r := NewRunner(config.Config{
		ProjectPath:        "/Users/dan/my-project",
		Image:              "myproject-agent-harness:latest",
		SessionIdleTimeout: 5 * time.Minute,
	}, log, "20260920-101500", false)
	r.pid = 4242
	r.sleep = func(time.Duration) {}
	r.run = func(args []string, opts session.Options) session.Outcome {
		seen = append(seen, launch{args: args, opts: opts})
		if len(seen) <= len(outcomes) {
			return outcomes[len(seen)-1]
		}
		return session.Outcome{}
	}
	return r, &seen
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// THE invariant this package exists to make unbreakable: Options.ContainerName
// must equal the `--name` in the argv, or the watchdog's timeout `docker kill`
// misses its target and the session runs unbounded. It used to be a comment
// restated at four separate stage call sites because nothing enforced it; now a
// caller cannot name a container at all, so the two are minted together.
func TestRunnerNameAlwaysMatchesTheArgv(t *testing.T) {
	r, seen := testRunner(t)

	r.Agent(AgentRun{Label: "implementation", Prompt: "do it", Cap: time.Minute})
	r.Shell(ShellRun{Label: "gate-check", Command: "go test ./...", WorktreePath: "/wt", Cap: time.Minute})

	if len(*seen) != 2 {
		t.Fatalf("launches = %d, want 2", len(*seen))
	}
	for _, l := range *seen {
		if got := flagValue(l.args, "--name"); got != l.opts.ContainerName {
			t.Errorf("--name %q != Options.ContainerName %q — the timeout kill would miss its target", got, l.opts.ContainerName)
		}
	}
}

// The model comes from the run, not the Runner, so each stage can pick its own.
func TestRunnerPinsTheRunsModel(t *testing.T) {
	r, seen := testRunner(t)

	r.Agent(AgentRun{Label: "implementation", Model: "claude-opus-5-5"})
	r.Agent(AgentRun{Label: "retrospective", Model: "claude-haiku-4-5-20251001"})

	for i, want := range []string{"claude-opus-5-5", "claude-haiku-4-5-20251001"} {
		if got := flagValue((*seen)[i].args, "--model"); got != want {
			t.Errorf("launch %d --model = %q, want %q", i, got, want)
		}
	}
}

// The name carries the Consumer-derived prefix (so one host running several
// projects never kills another project's containers), the run id, the pid (two
// runs in the same second stay distinct) and the role label.
func TestRunnerNameCarriesPrefixRunIDPidAndLabel(t *testing.T) {
	r, seen := testRunner(t)

	r.Agent(AgentRun{Label: "review"})

	want := sandbox.ContainerPrefix("/Users/dan/my-project") + "20260920-101500-4242-review"
	if got := (*seen)[0].opts.ContainerName; got != want {
		t.Errorf("container = %q, want %q", got, want)
	}
}

// One label mints both the container name and the transcript filename, so a
// reader can always find the log for a container (and the two can never drift).
// Agent sessions are stream-json (.jsonl); shell steps are piped tool stdout (.log).
func TestRunnerDerivesTheTranscriptFromTheSameLabel(t *testing.T) {
	r, seen := testRunner(t)

	agent := r.Agent(AgentRun{Label: "cifix-2"})
	shell := r.Shell(ShellRun{Label: "gate-lint", Command: "true"})

	if agent.Transcript != "cifix-2-20260920-101500.jsonl" {
		t.Errorf("agent transcript = %q", agent.Transcript)
	}
	if shell.Transcript != "gate-lint-20260920-101500.log" {
		t.Errorf("shell transcript = %q", shell.Transcript)
	}
	if (*seen)[0].opts.TranscriptFile != agent.Transcript || (*seen)[1].opts.TranscriptFile != shell.Transcript {
		t.Error("the transcript the Runner reports must be the one the session teed to")
	}
}

// A transient failure (the 137 OOM-kill, or a retryable exit-125 launch failure)
// is retried under a FRESH name: a wedged container's `--rm` teardown may have
// failed, leaving the old name taken.
func TestRunnerRetriesATransientFailureUnderAFreshName(t *testing.T) {
	r, seen := testRunner(t,
		session.Outcome{ExitCode: sandbox.ExitOOMKill},
		session.Outcome{ExitCode: 0},
	)

	res := r.Shell(ShellRun{
		Label: "prep", Command: "install",
		Retry: Retry{MaxAttempts: 3, Backoff: session.ConstantBackoff(time.Second)},
	})

	if res.ExitCode != 0 || res.Attempts != 2 {
		t.Fatalf("result = %+v, want the retry to have recovered on attempt 2", res)
	}
	names := []string{(*seen)[0].opts.ContainerName, (*seen)[1].opts.ContainerName}
	if names[0] == names[1] {
		t.Fatalf("both attempts used %q — a retry must not collide with the wedged container's name", names[0])
	}
	if !strings.HasSuffix(names[1], "-prep-retry2") {
		t.Errorf("retry container = %q, want the attempt suffixed", names[1])
	}
}

// The suffix is the caller's word, so the implementation stage's transient LAUNCH
// retries stay distinguishable from its usage-policy-refusal retries in both
// container names and transcripts.
func TestRunnerRetrySuffixIsTheCallersWord(t *testing.T) {
	r, seen := testRunner(t, session.Outcome{ExitCode: sandbox.ExitOOMKill}, session.Outcome{})

	r.Agent(AgentRun{
		Label: "implementation",
		Retry: Retry{MaxAttempts: 2, Backoff: session.ConstantBackoff(0), Suffix: "launch"},
	})

	if got := (*seen)[1].opts.ContainerName; !strings.HasSuffix(got, "-implementation-launch2") {
		t.Errorf("retry container = %q, want the `launch` suffix", got)
	}
}

// A real failure — a code the process itself returned — is final on the first
// attempt: retrying a failing test is not the transient class.
func TestRunnerDoesNotRetryARealFailure(t *testing.T) {
	r, seen := testRunner(t, session.Outcome{ExitCode: 2})

	res := r.Shell(ShellRun{Label: "gate-check", Command: "false", Retry: Retry{MaxAttempts: 4, Backoff: session.ConstantBackoff(0)}})

	if res.Attempts != 1 || len(*seen) != 1 {
		t.Errorf("attempts = %d / launches = %d, want a real failure to be final", res.Attempts, len(*seen))
	}
}

// The retry budget is a bound, not a suggestion.
func TestRunnerStopsAtTheRetryBudget(t *testing.T) {
	r, seen := testRunner(t)
	r.run = func([]string, session.Options) session.Outcome {
		return session.Outcome{ExitCode: sandbox.ExitOOMKill}
	}

	res := r.Shell(ShellRun{Label: "gate-check", Command: "x", Retry: Retry{MaxAttempts: 3, Backoff: session.ConstantBackoff(0)}})

	if res.Attempts != 3 {
		t.Errorf("attempts = %d, want the budget of 3", res.Attempts)
	}
	_ = seen
}

// Notify narrates each retry in the caller's own voice, AFTER the backoff has been
// slept — so the log line that says "retry 1/2 after 10s" is emitted at the moment
// the retry actually launches.
func TestRunnerNotifiesEachRetryWithTheSleptBackoff(t *testing.T) {
	r, _ := testRunner(t,
		session.Outcome{ExitCode: sandbox.ExitOOMKill},
		session.Outcome{ExitCode: sandbox.ExitOOMKill},
		session.Outcome{},
	)
	var order []string
	r.sleep = func(d time.Duration) { order = append(order, "slept "+d.String()) }

	r.Shell(ShellRun{
		Label: "gate-check", Command: "x",
		Retry: Retry{
			MaxAttempts: 3,
			Backoff:     session.ExponentialBackoff(30*time.Second, 120*time.Second),
			Notify: func(attempt int, waited time.Duration) {
				order = append(order, "notify attempt "+string(rune('0'+attempt))+" after "+waited.String())
			},
		},
	})

	want := []string{
		"slept 30s", "notify attempt 2 after 30s",
		"slept 1m0s", "notify attempt 3 after 1m0s",
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// The `--dry-run` preview comes from the same builder as the launch, so a printed
// plan can never show a command the harness would not actually run.
func TestRunnerPreviewMatchesTheLaunchedArgv(t *testing.T) {
	r, seen := testRunner(t)
	agent := AgentRun{Label: "implementation", Prompt: "do it", FindingsDir: "/logs/findings"}
	shell := ShellRun{Label: "gate-check", Command: "go test ./...", WorktreePath: "/wt"}

	preview := [][]string{r.AgentPreview(agent), r.ShellPreview(shell)}
	r.Agent(agent)
	r.Shell(shell)

	for i, l := range *seen {
		if !reflect.DeepEqual(preview[i], l.args) {
			t.Errorf("preview[%d] = %v, want the launched argv %v", i, preview[i], l.args)
		}
	}
}

// The zero Retry runs exactly once — what a caller with its own higher-level retry
// (the implementation stage's refusal loop) wants.
func TestRunnerZeroRetryRunsOnce(t *testing.T) {
	r, seen := testRunner(t, session.Outcome{ExitCode: sandbox.ExitOOMKill})

	res := r.Agent(AgentRun{Label: "retrospective"})

	if res.Attempts != 1 || len(*seen) != 1 {
		t.Errorf("attempts = %d / launches = %d, want exactly one", res.Attempts, len(*seen))
	}
}
