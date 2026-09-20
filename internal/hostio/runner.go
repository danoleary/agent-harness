package hostio

import (
	"fmt"
	"os"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// Runner is the sandbox half of [Real]: it owns {cfg, log, runID, prefix} and is
// the single place a container is named. Because it mints the `--name` AND builds
// the argv that carries it AND the session.Options the watchdog kills through,
// the "Options.ContainerName must equal the --name in the argv" invariant — which
// used to be restated in four separate comments across the stages because nothing
// enforced it — is now structural: there is no way to pass one without the other.
//
// It also owns the per-attempt naming of transient-failure retries, which was
// open-coded at five call sites with three different suffix conventions.
type Runner struct {
	cfg    config.Config
	log    *runlog.Logger
	runID  string
	prefix string
	// pid disambiguates two runs started in the same (second-resolution) runID, so
	// their containers get distinct names and distinct `docker kill` targets.
	pid     int
	verbose bool
	// run and sleep are the injected effects, so the naming and retry logic above
	// is unit-testable without a Docker daemon or a real wait.
	run   func(dockerArgs []string, opts session.Options) session.Outcome
	sleep func(time.Duration)
}

// NewRunner binds a Runner to one run of one ticket.
func NewRunner(cfg config.Config, log *runlog.Logger, runID string, verbose bool) *Runner {
	return &Runner{
		cfg:     cfg,
		log:     log,
		runID:   runID,
		prefix:  sandbox.ContainerPrefix(cfg.ProjectPath),
		pid:     os.Getpid(),
		verbose: verbose,
		run:     session.Run,
		sleep:   time.Sleep,
	}
}

// RunID is the run this Runner stamps into every name it mints.
func (r *Runner) RunID() string { return r.runID }

// name is the container name for one labelled run: the Consumer-derived prefix
// (so one host running several projects never kills another's containers), the
// run id, the pid, and the role label.
func (r *Runner) name(label string) string {
	return fmt.Sprintf("%s%s-%d-%s", r.prefix, r.runID, r.pid, label)
}

// Agent launches one sandboxed claude session and returns its outcome together
// with the names it was given.
func (r *Runner) Agent(a AgentRun) Result {
	return r.attempts(a.Label, a.Retry, func(label string) Result {
		name := r.name(label)
		transcript := runlog.TranscriptName(label, r.runID)
		out := r.run(r.agentArgs(name, a), session.Options{
			ContainerName:  name,
			TranscriptFile: transcript,
			Timeout:        a.Cap,
			IdleTimeout:    r.cfg.SessionIdleTimeout,
			Verbose:        r.verbose,
			Log:            r.log,
		})
		return Result{Outcome: out, Container: name, Transcript: transcript}
	})
}

// Shell launches one secret-free throwaway container running a command in a
// worktree. Its log is piped tool stdout, not a stream-json transcript, so it is
// stamped with a StepFooter — without it an OOM-kill's mid-line truncation is
// indistinguishable from a clean finish (BEH-537).
func (r *Runner) Shell(s ShellRun) Result {
	return r.attempts(s.Label, s.Retry, func(label string) Result {
		name := r.name(label)
		transcript := runlog.StepLogName(label, r.runID)
		out := r.run(r.shellArgs(name, s), session.Options{
			ContainerName:  name,
			TranscriptFile: transcript,
			Timeout:        s.Cap,
			IdleTimeout:    r.cfg.SessionIdleTimeout,
			Verbose:        r.verbose,
			Log:            r.log,
		})
		r.log.TeeLine(transcript, runlog.StepFooter(out.ExitCode))
		return Result{Outcome: out, Container: name, Transcript: transcript}
	})
}

// AgentPreview is the argv Agent would launch, for `--dry-run`. It goes through
// the same builder, so a printed plan can never drift from the real command.
func (r *Runner) AgentPreview(a AgentRun) []string {
	return r.agentArgs(r.name(a.Label), a)
}

// ShellPreview is the argv Shell would launch, for `--dry-run`.
func (r *Runner) ShellPreview(s ShellRun) []string {
	return r.shellArgs(r.name(s.Label), s)
}

// Preflight verifies the harness can actually launch a sandbox for this Consumer
// before a stage commits to a run (claiming the ticket, mutating tracker state).
func (r *Runner) Preflight() error {
	return sandbox.Preflight(sandbox.PreflightFor(r.cfg.Image, r.cfg.ProjectPath, r.cfg.Dockerfile))
}

func (r *Runner) agentArgs(name string, a AgentRun) []string {
	return sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:          r.cfg.Image,
		ProjectPath:    r.cfg.ProjectPath,
		FindingsDir:    a.FindingsDir,
		CacheVolume:    r.cfg.CacheVolume,
		CacheMountPath: r.cfg.CacheMountPath,
		Prompt:         a.Prompt,
		Model:          r.cfg.Model,
		ContainerName:  name,
	})
}

func (r *Runner) shellArgs(name string, s ShellRun) []string {
	return sandbox.BuildWorktreeCommandArgs(sandbox.GateConfig{
		Image:          r.cfg.Image,
		ProjectPath:    r.cfg.ProjectPath,
		WorktreePath:   s.WorktreePath,
		CacheVolume:    r.cfg.CacheVolume,
		CacheMountPath: r.cfg.CacheMountPath,
		ContainerName:  name,
	}, s.Command)
}

// attempts runs launch under the retry schedule, suffixing the label per attempt
// so every retry gets a fresh container name (a wedged container's `--rm`
// teardown may have failed, leaving the old name taken) and its own transcript.
// It stops at the first non-transient outcome — a success, or a real failure the
// process itself returned — exactly as session.RetryTransient did at each of the
// call sites this replaces.
func (r *Runner) attempts(baseLabel string, retry Retry, launch func(label string) Result) Result {
	maxAttempts := retry.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	suffix := retry.Suffix
	if suffix == "" {
		suffix = "retry"
	}

	var res Result
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		label := baseLabel
		if attempt > 1 {
			label = fmt.Sprintf("%s-%s%d", baseLabel, suffix, attempt)
		}
		res = launch(label)
		res.Attempts = attempt
		if !res.Retryable() || attempt == maxAttempts {
			return res
		}
		var waited time.Duration
		if retry.Backoff != nil {
			waited = retry.Backoff(attempt)
		}
		r.sleep(waited)
		if retry.Notify != nil {
			retry.Notify(attempt+1, waited)
		}
	}
	return res
}
