// Package fix is the host-side machinery that repairs a branch which already
// exists: re-running the Consumer's Gates over its worktree, the sandboxed CIFix
// session the CI watch hands a red check to, and the sandboxed conflict
// resolution that brings a branch back onto origin/main. Each ends in a
// ground-truth check the host makes for itself (ADR-0016) — a green Gate exit, a
// committed fix, a branch actually on its base — never the agent's report.
//
// Two callers share it, so neither copies it (ADR-0019): the review stage, on the
// branch it has just reviewed, and the PR loop, on an open PR that went red or
// conflicting after its review finished.
package fix

import (
	"fmt"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/verify"
)

// gateOOM* bound the OOM-kill retry of the heavier host-side gate (install + check
// + build) separately (BEH-530). A full build's footprint is far larger than an
// install's, so the install's short fixed beat doesn't transfer: on BEH-501 three
// back-to-back retries 10s apart died progressively EARLIER (memory pressure
// compounding, not recovering). An exponential backoff (30s → 60s → 120s) over one
// more attempt gives the starved Docker VM real time to reclaim before each retry.
const (
	gateOOMMaxAttempts = 4
	gateOOMBackoffBase = 30 * time.Second
	gateOOMBackoffCap  = 120 * time.Second
)

// Env is what every fix acts through: the run's Host, the Consumer's config, the
// run log, and the name the narration speaks in ("review", "pr-fix").
type Env struct {
	Host hostio.Host
	Cfg  config.Config
	Log  *runlog.Logger
	Who  string
}

// Target is the branch a fix session works on: the worktree slug the Host keys it
// by, and the ticket (or PR) whose intent the prompt carries.
type Target struct {
	Slug   string
	Ticket ticket.Ticket
	// Branch, when set, is what the prompt calls the branch, in place of
	// `<branch_prefix>/<slug>`. The PR loop sets it: it works a branch it did not
	// name.
	Branch string
}

func (e Env) promptContext(t Target) prompt.Context {
	return prompt.Context{
		Ticket: t.Ticket, Slug: t.Slug, BranchPrefix: e.Cfg.BranchPrefix, Branch: t.Branch,
		Tracker: e.Cfg.Tracker.Kind, WorktreePath: e.Host.WorktreePath(t.Slug),
	}
}

// --- gates -------------------------------------------------------------------

// GateResult is the outcome of re-running the config-declared named gate list
// host-side (BEH-634). Outcome is the failing gate's session outcome, or — when
// every gate is green — the last gate's. FailedGate names the first gate that
// exited non-zero (empty when all passed); it is what the log surfaces and what
// flows into the CI-fix diagnosis, matching how internal/ci names a failing check.
type GateResult struct {
	Outcome    session.Outcome
	FailedGate string
}

// Green reports whether every gate passed.
func (g GateResult) Green() bool { return g.Outcome.ExitCode == 0 }

// RunGates runs each configured named gate in order via runOne, stopping at the
// first non-zero exit. runOne launches one gate's throwaway container (with its
// own OOM retry) and returns its outcome; RunGates is the pure orchestration over
// it, so it is unit-testable with an injected runner. An empty gate list — a
// Consumer misconfiguration guarded upstream by config.validate — yields a green
// zero result. The first red gate's name is surfaced so it can flow into the log
// + CI-fix diagnosis; the gates after it are skipped (the push gate only needs the
// first red to withhold the push).
func RunGates(gates []config.Gate, runOne func(config.Gate) session.Outcome) GateResult {
	var last session.Outcome
	for _, g := range gates {
		last = runOne(g)
		if last.ExitCode != 0 {
			return GateResult{Outcome: last, FailedGate: g.Name}
		}
	}
	return GateResult{Outcome: last}
}

// GateRun is the container one Gate runs in. labelPrefix distinguishes a re-gate
// (after a review re-launch or a resolved conflict) from the first gate run.
func GateRun(cfg config.Config, worktreePath, labelPrefix string, g config.Gate) hostio.ShellRun {
	return hostio.ShellRun{Label: labelPrefix + "-" + g.Name, Command: g.Command, WorktreePath: worktreePath, Cap: cfg.ReviewTimeout}
}

// Gates re-runs the config-declared named gate list host-side in throwaway
// containers — one per gate, in order, stopping at the first red (BEH-634) — and
// reports the first failing gate by name. Each gate runs under its own OOM-retry
// (BEH-530). labelPrefix keys the run so it can happen more than once per stage
// (a resolved conflict re-gates the rewritten tree — BEH-581).
//
// The gate's exit code is the ONLY thing that authorises a push — never the
// agent's report. A 137 OOM-kill is environmental, never the diff: the gate's own
// install (and even a typecheck) can be SIGKILLed under memory pressure, which
// would flip a genuinely green branch red and push nothing. A real gate failure
// returns a non-137 code and is final on the first attempt.
func (e Env) Gates(slug, labelPrefix string) GateResult {
	// A docs-only diff (root markdown / docs/**) feeds none of the gates, so the heavy
	// host re-run validates nothing a prose edit could break — skip it and treat the
	// gate as green (BEH-687). Read fresh here, not once up front: an earlier
	// session may have committed a code edit, and BranchDocsOnly over the current
	// tree (which `git diff origin/main` reads including uncommitted work) reflects
	// that — fail-safe to running the full gate on any doubt.
	if e.Host.BranchDocsOnly(slug) {
		e.Log.Event(e.Who + " · host gate skipped — diff touches only docs/prose paths no gate depends on (BEH-687)")
		return GateResult{}
	}
	worktreePath := e.Host.WorktreePath(slug)
	return RunGates(e.Cfg.Gates, func(g config.Gate) session.Outcome {
		e.Log.Event(fmt.Sprintf("%s · gate %q: %s", e.Who, g.Name, g.Command))
		run := GateRun(e.Cfg, worktreePath, labelPrefix, g)
		run.Retry = hostio.Retry{
			MaxAttempts: gateOOMMaxAttempts,
			Backoff:     session.ExponentialBackoff(gateOOMBackoffBase, gateOOMBackoffCap),
			Notify: func(attempt int, waited time.Duration) {
				e.Log.Event(fmt.Sprintf(
					"%s ↻ host-side gate %q OOM-killed (exit 137) — retry %d/%d after %s (BEH-530)",
					e.Who, g.Name, attempt-1, gateOOMMaxAttempts-1, waited,
				))
			},
		}
		return e.Host.Shell(run).Outcome
	})
}

// --- CI ----------------------------------------------------------------------

// CI returns the CI watch's fix callback: it launches a sandboxed Claude session
// over the existing worktree, steered by the CIFix prompt with the fetched failing
// logs, then enforces ground truth — a non-zero session exit or a worktree left
// dirty (the agent didn't commit) is a failure, so the harness never pushes an
// unverified or self-reported-only fix. Each attempt gets a unique label, from
// which the Runner mints the container name and transcript.
func (e Env) CI(t Target) func(ciLogs string, logAvailable bool) error {
	attempt := 0
	return func(ciLogs string, logAvailable bool) error {
		attempt++
		// Capture the tip before the session so we can tell afterwards whether the
		// agent actually committed a fix or correctly concluded there was nothing to
		// fix (BEH-561). A read failure leaves headBefore empty, which degrades to
		// "treat any HEAD as a real commit" — never a spurious empty commit.
		headBefore, _ := e.Host.HeadSHA(t.Slug)
		e.Log.Event(fmt.Sprintf("CI red — launching auto-fix session %d (cap %d min active)", attempt, int(e.Cfg.ReviewTimeout.Minutes())))
		pctx := e.promptContext(t)
		pctx.CILogs, pctx.CILogAvailable = ciLogs, logAvailable
		res := e.Host.Agent(hostio.AgentRun{
			Label:  fmt.Sprintf("cifix-%d", attempt),
			Model:  e.Cfg.ReviewModel,
			Prompt: prompt.For(prompt.CIFix, pctx),
			Cap:    e.Cfg.ReviewTimeout,
		})
		if err := sessionError(res.Outcome, attempt); err != nil {
			return err
		}
		// Ground truth, never the agent's say-so: the fix must be committed (clean
		// worktree) or the harness has nothing trustworthy to push.
		if !e.Host.WorktreeClean(t.Slug) {
			return fmt.Errorf("auto-fix session %d left uncommitted changes — not pushing", attempt)
		}
		// The agent may have correctly concluded the red is not a code defect (a
		// cancelled/superseded/flaky run) and committed nothing. Don't force a
		// speculative diff: add an empty commit so the re-push gives CI a fresh HEAD
		// to re-run against; a no-op if a real fix moved HEAD (BEH-561).
		if err := e.Host.EnsureCIRerunCommit(t.Slug, headBefore); err != nil {
			return fmt.Errorf("auto-fix session %d: re-trigger commit failed: %w", attempt, err)
		}
		return nil
	}
}

// sessionError maps a finished auto-fix session's outcome to the error the
// ci.Driver.Fix callback should return — nil if the session ran cleanly and the
// caller should proceed to the ground-truth (committed-fix) checks. A spending-cap
// abort (BEH-571) is its own retry-after-reset class, so it is surfaced as
// ci.ErrSpendingCapActive and takes precedence over the exit code (a cap abort
// also exits non-zero) — letting WatchAndFix defer rather than count the 1-second
// no-op as a fix-attempt failure.
func sessionError(outcome session.Outcome, attempt int) error {
	if outcome.SpendingCapAbort {
		return ci.ErrSpendingCapActive
	}
	if outcome.ExitCode != 0 {
		return fmt.Errorf("auto-fix session %d exited %d", attempt, outcome.ExitCode)
	}
	return nil
}

// --- conflicts ---------------------------------------------------------------

// Strategy is how a conflict resolution brings a branch onto origin/main.
type Strategy int

const (
	// Rebase replays the branch onto origin/main, rewriting its history; the caller
	// pushes it force-with-lease. Only for a branch the harness owns.
	Rebase Strategy = iota
	// Merge merges origin/main into the branch, keeping its history; the caller
	// pushes it normally. For a branch a human opted in, where a force-push would
	// rewrite history someone else may have checked out (ADR-0019).
	Merge
)

func (s Strategy) String() string {
	if s == Merge {
		return "merge"
	}
	return "rebase"
}

// Conflict is one conflict to resolve: the branch, how to bring it onto its base,
// and the gate re-run that must be green over the resolved tree before the caller
// may push.
type Conflict struct {
	Target
	Strategy Strategy
	Regate   func() GateResult
}

// ConflictResult is how a conflict resolution ended. OK authorises the push.
type ConflictResult struct {
	OK bool
	// SpendingCapAbort marks a session the spending cap killed before it resolved
	// anything: retry after reset, not a conflict a human must look at.
	SpendingCapAbort bool
	// Reason says why it failed: the ground-truth verdict over the worktree, or the
	// red re-gate.
	Reason string
	// Resolved marks a resolution the ground truth accepted; when OK is still false
	// it was the re-gate over the resolved tree that failed, and Gate says how.
	Resolved bool
	// Gate is the re-gate over the resolved tree; zero when nothing was re-gated.
	Gate GateResult
}

// ResolveConflict launches a bounded sandboxed conflict-resolution session over the
// existing worktree (steered by the RebaseFix prompt to bring the branch onto
// origin/main by c.Strategy, resolve, and commit), then enforces ground truth — the
// verdict is never the agent's say-so but verify.RebaseResolution over the
// worktree's git state (session exit, clean tree, origin/main an ancestor of the
// tip). On a clean resolution it re-runs the gates over the rewritten tree and
// only reports OK when they are green and the worktree still clean. A failed
// session may have left the worktree mid-replay, so it is restored to a clean,
// on-branch state for the next resume.
func (e Env) ResolveConflict(c Conflict) ConflictResult {
	e.Log.Event(fmt.Sprintf("launching conflict-resolution session (%s onto origin/main, cap %d min active)", c.Strategy, int(e.Cfg.ReviewTimeout.Minutes())))
	pctx := e.promptContext(c.Target)
	pctx.Merge = c.Strategy == Merge
	res := e.Host.Agent(hostio.AgentRun{
		Label:  "rebasefix",
		Model:  e.Cfg.ReviewModel,
		Prompt: prompt.For(prompt.RebaseFix, pctx),
		Cap:    e.Cfg.ReviewTimeout,
	})
	e.Log.Event(fmt.Sprintf("conflict-resolution session exited (code %d)", res.ExitCode))

	verdict := verify.RebaseResolution(e.Host, c.Slug, res.Outcome)
	if !verdict.OK {
		e.Host.AbortRebase(c.Slug)
		if verdict.SpendingCapAbort {
			e.Log.Event(e.Who + " ↻ conflict resolution deferred — spending cap reached, retry after reset (BEH-494) — keeping worktree")
			return ConflictResult{SpendingCapAbort: true, Reason: verdict.Reason}
		}
		e.Log.Event(e.Who + " ✗ " + verdict.Reason + " — keeping worktree, nothing pushed")
		return ConflictResult{Reason: verdict.Reason}
	}
	e.Log.Event(e.Who + " ✓ " + verdict.Reason)

	// The resolution rewrote the tree, so any earlier gate result is stale — re-run
	// the gates before trusting the push. Push only on a green gate over a
	// still-clean worktree.
	gate := c.Regate()
	if !gate.Green() || !e.Host.WorktreeClean(c.Slug) {
		reason := fmt.Sprintf("post-resolution gate re-run failed (exit %d)", gate.Outcome.ExitCode)
		e.Log.Event(e.Who + " ✗ " + reason + " — keeping worktree, nothing pushed")
		return ConflictResult{Resolved: true, Reason: reason, Gate: gate}
	}
	e.Log.Event(e.Who + " ✓ post-resolution gate re-run is green — clear to push")
	return ConflictResult{OK: true, Resolved: true, Gate: gate}
}
