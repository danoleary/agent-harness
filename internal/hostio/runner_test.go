package hostio

import (
	"errors"
	"os"
	"path/filepath"
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
		Host: config.Host{
			ProjectPath:        "/Users/dan/my-project",
			SessionIdleTimeout: 5 * time.Minute,
		},
		Project: config.Project{
			Image: "myproject-agent-harness:latest",
		},
	}, log, "20260920-101500", false)
	r.pid = "4242"
	r.sleep = func(time.Duration) {}
	r.skillDrift = func() ([]string, error) { return nil, nil }
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

// #305: an agent session's stderr goes to a sibling .stderr.log, so a CLI warning
// can never be line 1 of its .jsonl transcript. A shell step keeps its stderr in
// its step log, beside the stdout it interleaves with.
func TestRunnerKeepsAnAgentsStderrBesideItsTranscript(t *testing.T) {
	r, seen := testRunner(t)

	r.Agent(AgentRun{Label: "implementation"})
	r.Shell(ShellRun{Label: "gate-lint", Command: "true"})

	if got := (*seen)[0].opts.StderrFile; got != "implementation-20260920-101500.stderr.log" {
		t.Errorf("agent stderr file = %q", got)
	}
	if got := (*seen)[1].opts.StderrFile; got != "" {
		t.Errorf("shell stderr file = %q, want none: its stderr belongs in its step log", got)
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

// A dry-run plan previews through the same Runner, so the Runner still mints the
// name — it just has no run to stamp it with yet, so it stamps placeholders. The
// preview host resolves the ticket's worktree like the real one and launches
// nothing (it has no Agent or Shell to call).
func TestPreviewNamesContainersWithPlaceholders(t *testing.T) {
	cfg := config.Config{
		Host:    config.Host{ProjectPath: "/Users/dan/my-project"},
		Project: config.Project{Image: "myproject-agent-harness:latest", BranchPrefix: "feat"},
	}
	p := NewPreview(cfg, "proj-1")

	agent := p.AgentPreview(AgentRun{Label: "implementation", Model: "claude-opus-5-5"})
	shell := p.ShellPreview(ShellRun{Label: "gate-check", Command: "go test ./...", WorktreePath: "/wt"})

	prefix := sandbox.ContainerPrefix("/Users/dan/my-project")
	if got, want := flagValue(agent, "--name"), prefix+"<run-id>-<pid>-implementation"; got != want {
		t.Errorf("agent preview --name = %q, want %q", got, want)
	}
	if got, want := flagValue(shell, "--name"), prefix+"<run-id>-<pid>-gate-check"; got != want {
		t.Errorf("shell preview --name = %q, want %q", got, want)
	}
	if got, want := p.Worktree().Branch(), "feat/proj-1"; got != want {
		t.Errorf("preview branch = %q, want %q", got, want)
	}
}

// branchDockerfileRunner is a testRunner over a real checkout + worktree on disk
// whose sandbox Dockerfiles are checkoutBody and worktreeBody, with image builds
// recorded (and failed with buildErr) rather than run.
func branchDockerfileRunner(t *testing.T, checkoutBody, worktreeBody string, buildErr error) (r *Runner, wt string, seen *[]launch, ensured *[]sandbox.PreflightConfig) {
	t.Helper()
	const rel = ".agent-harness/Dockerfile"
	checkout := t.TempDir()
	wt = filepath.Join(checkout, ".claude", "worktrees", "proj-1")
	for root, body := range map[string]string{checkout: checkoutBody, wt: worktreeBody} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, seen = testRunner(t)
	r.cfg.ProjectPath = checkout
	r.cfg.Dockerfile = rel
	var calls []sandbox.PreflightConfig
	r.ensureImage = func(pc sandbox.PreflightConfig) error {
		calls = append(calls, pc)
		return buildErr
	}
	return r, wt, seen, &calls
}

// imageOf is the image a recorded worktree-command argv runs: the word before `bash -c`.
func imageOf(args []string) string {
	i := slices.Index(args, "bash")
	if i < 1 {
		return ""
	}
	return args[i-1]
}

// #61: a branch that fixes its own sandbox Dockerfile has its prep and gate run in
// an image built from the worktree's Dockerfile — never the checkout's pre-change
// one, where the gate would go red for exactly the reason the branch fixes.
func TestShellRunsABranchThatChangesTheDockerfileInItsOwnImage(t *testing.T) {
	r, wt, seen, ensured := branchDockerfileRunner(t, "FROM base:0.5.0\n", "FROM base:0.5.0\nRUN install-go\n", nil)
	want, _ := sandbox.WorktreeImage(r.cfg.Image, r.cfg.ProjectPath, wt, r.cfg.Dockerfile)

	r.Shell(ShellRun{Label: "prep", Command: "go mod download", WorktreePath: wt, Cap: time.Minute})
	r.Shell(ShellRun{Label: "gate-check", Command: "make check", WorktreePath: wt, Cap: time.Minute})

	for _, l := range *seen {
		if got := imageOf(l.args); got != want {
			t.Errorf("worktree command ran in %q, want the branch image %q", got, want)
		}
	}
	if len(*ensured) != 1 {
		t.Fatalf("branch image ensured %d times, want once per run", len(*ensured))
	}
	pc := (*ensured)[0]
	if pc.Image != want || pc.Dockerfile != filepath.Join(wt, r.cfg.Dockerfile) || pc.BuildContext != filepath.Join(wt, ".agent-harness") {
		t.Errorf("branch image built as %q from %q in %q; want %q from the worktree's Dockerfile", pc.Image, pc.Dockerfile, pc.BuildContext, want)
	}
}

func TestShellKeepsTheCheckoutImageWhenTheBranchLeavesTheDockerfileAlone(t *testing.T) {
	r, wt, seen, ensured := branchDockerfileRunner(t, "FROM base:0.5.0\n", "FROM base:0.5.0\n", nil)

	r.Shell(ShellRun{Label: "gate-check", Command: "make check", WorktreePath: wt, Cap: time.Minute})

	if got := imageOf((*seen)[0].args); got != r.cfg.Image {
		t.Errorf("worktree command ran in %q, want the checkout image %q", got, r.cfg.Image)
	}
	if len(*ensured) != 0 {
		t.Errorf("built %d branch images for an unchanged Dockerfile, want none", len(*ensured))
	}
}

// A branch image that will not build is reported as that, not as a red gate to go
// debug in the code — and the failed multi-minute build is not repeated per gate.
func TestShellReportsABranchImageThatFailsToBuild(t *testing.T) {
	r, wt, seen, ensured := branchDockerfileRunner(t, "FROM base:0.5.0\n", "FROM base:nope\n", errors.New("pull access denied for base"))

	first := r.Shell(ShellRun{Label: "prep", Command: "go mod download", WorktreePath: wt, Cap: time.Minute})
	second := r.Shell(ShellRun{Label: "gate-check", Command: "make check", WorktreePath: wt, Cap: time.Minute})

	if len(*seen) != 0 {
		t.Errorf("launched %d containers without a branch image, want none", len(*seen))
	}
	if len(*ensured) != 1 {
		t.Errorf("branch image build attempted %d times, want once", len(*ensured))
	}
	for _, res := range []Result{first, second} {
		if res.ExitCode != sandbox.ExitCannotStart || res.Retryable() {
			t.Errorf("exit %d (retryable %v), want a terminal %d", res.ExitCode, res.Retryable(), sandbox.ExitCannotStart)
		}
		for _, want := range []string{"branch modifies the sandbox Dockerfile", "pull access denied"} {
			if !strings.Contains(res.StderrTail, want) {
				t.Errorf("StderrTail %q does not say %q", res.StderrTail, want)
			}
		}
	}
}

// The dry-run plan names the branch image the gates would run in, without building it.
func TestShellPreviewNamesTheBranchImageWithoutBuildingIt(t *testing.T) {
	r, wt, _, ensured := branchDockerfileRunner(t, "FROM base:0.5.0\n", "FROM base:0.6.0\n", nil)
	want, _ := sandbox.WorktreeImage(r.cfg.Image, r.cfg.ProjectPath, wt, r.cfg.Dockerfile)

	if got := imageOf(r.ShellPreview(ShellRun{Label: "gate-check", Command: "make check", WorktreePath: wt})); got != want {
		t.Errorf("preview runs %q, want the branch image %q", got, want)
	}
	if len(*ensured) != 0 {
		t.Errorf("preview built %d images, want none", len(*ensured))
	}
}

// #219: a run's warnings — a prompt body with a placeholder the binary does not
// know, a retrospective with no transcripts — are narrated before the session
// launches, so the run log says why a prompt came out hollow.
func TestRunnerNarratesTheRunsWarningsBeforeLaunching(t *testing.T) {
	r, seen := testRunner(t)

	r.Agent(AgentRun{Label: "retrospective", Warnings: []string{"prompt body names {{.RunName}}"}})

	if len(*seen) != 1 {
		t.Fatalf("launches = %d, want 1", len(*seen))
	}
	raw, err := os.ReadFile(filepath.Join(r.log.Dir, "run.jsonl"))
	if err != nil {
		t.Fatalf("read run.jsonl: %v", err)
	}
	if !strings.Contains(string(raw), `⚠ retrospective: prompt body names {{.RunName}}`) {
		t.Errorf("run.jsonl does not narrate the warning:\n%s", raw)
	}
}

// runLog is what the Runner narrated into run.jsonl.
func runLog(t *testing.T, r *Runner) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.log.Dir, "run.jsonl"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read run.jsonl: %v", err)
	}
	return string(raw)
}

// #245: a ticket's session starts in its worktree, so the skills it loads are the
// base commit's. A worktree that is there needs no look at the host checkout.
func TestAgentStartsInItsWorktree(t *testing.T) {
	r, seen := testRunner(t)
	probed := false
	r.skillDrift = func() ([]string, error) { probed = true; return []string{".claude/skills/tdd/SKILL.md"}, nil }
	wt := t.TempDir()

	r.Agent(AgentRun{Label: "implementation", WorkDir: wt})

	if got := flagValue((*seen)[0].args, "-w"); got != wt {
		t.Errorf("-w = %q, want the worktree %q", got, wt)
	}
	if probed {
		t.Error("a session in its worktree probed the host checkout's .claude/ tree")
	}
}

// The incident in #245: the session started in the operator's host checkout, which
// sat on a side branch whose /tdd skill predated the base commit's, and nothing
// said so. A session that starts in the checkout now narrates that drift.
func TestAgentInTheCheckoutWarnsWhenItsSkillsDifferFromTheBase(t *testing.T) {
	r, seen := testRunner(t)
	r.skillDrift = func() ([]string, error) {
		return []string{".claude/skills/review-worktree/SKILL.md", ".claude/skills/tdd/SKILL.md"}, nil
	}

	r.Agent(AgentRun{Label: "triage"})

	if got := flagValue((*seen)[0].args, "-w"); got != r.cfg.ProjectPath {
		t.Errorf("-w = %q, want the checkout %q", got, r.cfg.ProjectPath)
	}
	log := runLog(t, r)
	for _, want := range []string{"⚠ triage:", "origin/main", ".claude/skills/tdd/SKILL.md", ".claude/skills/review-worktree/SKILL.md"} {
		if !strings.Contains(log, want) {
			t.Errorf("run.jsonl does not name %q:\n%s", want, log)
		}
	}
}

// A checkout whose .claude/ matches the base, or that cannot be compared (no
// origin/main yet), says nothing: the warning is for drift, not for doubt.
func TestAgentInTheCheckoutIsQuietWithoutDrift(t *testing.T) {
	for name, probe := range map[string]func() ([]string, error){
		"matches":    func() ([]string, error) { return nil, nil },
		"unreadable": func() ([]string, error) { return nil, errors.New("unknown revision origin/main") },
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := testRunner(t)
			r.skillDrift = probe
			r.Agent(AgentRun{Label: "triage"})
			if log := runLog(t, r); strings.Contains(log, "⚠") {
				t.Errorf("run.jsonl warns:\n%s", log)
			}
		})
	}
}

// A worktree that has gone from disk must not be recreated as an empty directory
// by `docker run -w` inside the mounted checkout: the session starts in the
// checkout instead, and says so along with any skill drift there.
func TestAgentWhoseWorktreeIsGoneStartsInTheCheckoutAndSaysSo(t *testing.T) {
	r, seen := testRunner(t)
	r.skillDrift = func() ([]string, error) { return []string{".claude/skills/tdd/SKILL.md"}, nil }
	gone := filepath.Join(t.TempDir(), "beh-1")

	r.Agent(AgentRun{Label: "retrospective", WorkDir: gone})

	if got := flagValue((*seen)[0].args, "-w"); got != r.cfg.ProjectPath {
		t.Errorf("-w = %q, want the checkout %q", got, r.cfg.ProjectPath)
	}
	log := runLog(t, r)
	for _, want := range []string{gone, ".claude/skills/tdd/SKILL.md"} {
		if !strings.Contains(log, want) {
			t.Errorf("run.jsonl does not name %q:\n%s", want, log)
		}
	}
}

// A dry-run plan shows the worktree the session will start in, though a fresh
// ticket's worktree does not exist yet when the plan is printed.
func TestAgentPreviewStartsInTheWorktree(t *testing.T) {
	r, _ := testRunner(t)
	wt := "/Users/dan/my-project/.claude/worktrees/proj-1"
	if got := flagValue(r.AgentPreview(AgentRun{Label: "implementation", WorkDir: wt}), "-w"); got != wt {
		t.Errorf("preview -w = %q, want %q", got, wt)
	}
}
