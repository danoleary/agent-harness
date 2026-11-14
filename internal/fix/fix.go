// Package fix is the host-side machinery that repairs a branch which already
// exists: re-running the Consumer's Gates over its worktree, the sandboxed CIFix
// session the CI watch hands a red check to, and the sandboxed conflict
// resolution that brings a branch back onto origin/main. Each ends in a
// ground-truth check the host makes for itself (ADR-0016) — a green Gate exit, a
// committed fix, a branch actually on its base — never the agent's report.
//
// Its callers share it, so none copies it (ADR-0019): the review stage, on the
// branch it has just reviewed; the PR loop, on an open PR that went red or
// conflicting after its review finished; and the security-alerts loop, which
// provisions and gates the branch it cuts for a package and fixes the red CI of
// the PR it opens (ADR-0040).
package fix

import (
	"fmt"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/hostlock"
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
	// SkipWhenOnly makes the gates honour each Gate's skip_when_only: a gate whose
	// list matches every path of the diff against origin/main is skipped, and CI is
	// left to check it before merge (#221, ADR-0049). Only the PR loop sets it; the
	// stages and the security-alerts loop run every gate.
	SkipWhenOnly bool
}

// Target is the branch a fix session works on: the slug the prompt names it by,
// and the ticket (or PR) whose intent the prompt carries. The worktree itself is
// the Host's, which is bound to the same ticket.
type Target struct {
	Slug   string
	Ticket ticket.Ticket
	// Branch, when set, is what the prompt calls the branch, in place of
	// `<branch_prefix>/<slug>`. The PR loop sets it: it works a branch it did not
	// name.
	Branch string
	// Checkpoint, when set, is the recovery checkpoint the worktree starts from: an
	// earlier attempt's saved, unverified work (#246), which the prompt names.
	Checkpoint *prompt.Checkpoint
}

func (e Env) promptContext(t Target) prompt.Context {
	return prompt.Context{
		Ticket: t.Ticket, Slug: t.Slug, BranchPrefix: e.Cfg.BranchPrefix, Branch: t.Branch,
		Tracker: e.Cfg.Tracker.Kind, WorktreePath: e.Host.Worktree().Path(),
		Body: e.Cfg.Prompts.Fix, Checkpoint: t.Checkpoint,
	}
}

// --- provisioning ------------------------------------------------------------

// Provision runs the Consumer's post_create in the fresh worktree, so the sessions
// and the gates open onto installed dependencies. A failed install only warns (a
// session can install in-session), unless the shell could not run the command at
// all, which no session can fix (#46). Both loops that work a worktree the ticket
// pipeline did not create share it.
func (e Env) Provision() bool { return e.provisionIn(e.Host.Worktree().Path(), "prep") }

// provisionIn is [Env.Provision] over the worktree at path, under label.
func (e Env) provisionIn(path, label string) bool {
	if e.Cfg.PostCreate == "" {
		return true
	}
	res := e.Host.Shell(hostio.ShellRun{Label: label, Command: e.Cfg.PostCreate, WorktreePath: path, Cap: e.Cfg.ReviewTimeout})
	if missing, ok := verify.CommandUnrunnable(res.Outcome); ok {
		e.Log.Event(fmt.Sprintf("%s ✗ post_create could not run (%q, exit %d) — fix the sandbox image's PATH; nothing pushed", e.Who, missing, res.ExitCode))
		return false
	}
	if res.ExitCode != 0 {
		e.Log.Event(fmt.Sprintf("%s … warning: post_create exited %d — sessions will provision in-session if needed", e.Who, res.ExitCode))
	}
	return true
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
	// Log is the failing gate's step log, a filename under the ticket's log dir
	// (`gate-<name>-<run>.log`); empty when every gate passed. The review hands its
	// tail to the session it re-launches over a red gate (#111).
	Log string
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
// returns a non-137 code and is final unless its log carries an infrastructure
// signature, which earns it one re-run (#167).
func (e Env) Gates(labelPrefix string) GateResult { return e.gates(labelPrefix, nil) }

// GatesOver is [Env.Gates] over a fix whose commits changed fixed: a red gate whose
// failures name only files outside fixed is re-run once too, since a fix cannot
// have broken a file it never touched (#167). A failure in a file the fix changed
// stays final. Pass a non-nil fixed, empty for a fix that changed nothing.
func (e Env) GatesOver(labelPrefix string, fixed []string) GateResult {
	if fixed == nil {
		fixed = []string{}
	}
	return e.gates(labelPrefix, fixed)
}

// gates is Gates, with the files the fix changed when there is a fix to compare
// against (nil when there is none).
func (e Env) gates(labelPrefix string, fixed []string) GateResult {
	// A docs-only diff (root markdown / docs/**) feeds none of the gates, so the heavy
	// host re-run validates nothing a prose edit could break — skip it and treat the
	// gate as green (BEH-687). Read fresh here, not once up front: an earlier
	// session may have committed a code edit, and DocsOnly over the current tree
	// (which `git diff origin/main` reads including uncommitted work) reflects that —
	// fail-safe to running the full gate on any doubt.
	wt := e.Host.Worktree()
	if wt.DocsOnly(e.Cfg.DocsOnlyExcludedRoots) {
		e.Log.Event(e.Who + " · host gate skipped — diff touches only docs/prose paths no gate depends on (BEH-687)")
		return GateResult{}
	}
	gates := e.toRun(wt, true)
	if len(gates) == 0 {
		return GateResult{}
	}
	return e.gatesIn(wt.Path(), gates, labelPrefix, fixed)
}

// toRun is the gates to run over wt: all of them, less, when the Env honours
// skip_when_only, each gate whose list matches every path wt's diff against
// origin/main touches (#221). A diff that cannot be read, or is empty, skips
// nothing. narrate logs each skip and why.
func (e Env) toRun(wt gitpkg.Worktree, narrate bool) []config.Gate {
	if !e.SkipWhenOnly {
		return e.Cfg.Gates
	}
	paths, err := wt.DiffPaths()
	if err != nil {
		return e.Cfg.Gates
	}
	var run []config.Gate
	for _, g := range e.Cfg.Gates {
		if !g.SkipsOver(paths) {
			run = append(run, g)
			continue
		}
		if narrate {
			e.Log.Event(fmt.Sprintf("%s · gate %q skipped — every path the fix changes against %s matches its skip_when_only (%s); CI checks it before merge (#221)", e.Who, g.Name, gitpkg.BaseRef, strings.Join(g.SkipWhenOnly, ", ")))
		}
	}
	return run
}

// gatesIn runs gates over the worktree at worktreePath: the ticket's own, or a
// trial merge of it (#259).
func (e Env) gatesIn(worktreePath string, gates []config.Gate, labelPrefix string, fixed []string) GateResult {
	// Only so many runs gate at once across the loop instances sharing the lock
	// dir (#89). The wait is for the gate step alone: a run implements and reviews
	// while another gates, and queues here.
	slots := hostlock.Slots{Dir: e.Cfg.GateSlotsDir(), Limit: e.Cfg.GateConcurrency}
	release, err := slots.Acquire(func() {
		e.Log.Event(fmt.Sprintf("%s · waiting for a gate slot — %d run(s) may gate at once (GATE_CONCURRENCY) and another run is gating", e.Who, e.Cfg.GateConcurrency))
	})
	if err != nil {
		// A lock dir the harness cannot write is a broken host, not a reason to gate
		// unbounded or to fail the branch: say so, and gate.
		e.Log.Event(fmt.Sprintf("%s · gate slot unavailable (%v) — gating without the concurrency limit", e.Who, err))
		release = func() {}
	}
	defer release()
	// RunGates stops at the first red, so the last gate run is the failing one.
	var lastLog string
	res := RunGates(gates, func(g config.Gate) session.Outcome {
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
		r := e.Host.Shell(run)
		if why := rerunReason(r.Outcome, e.gateLog(r.Transcript), fixed); why != "" {
			e.Log.Event(fmt.Sprintf("%s ↻ gate %q is red (exit %d) with %s — re-running it once before it counts as red (#167)", e.Who, g.Name, r.ExitCode, why))
			run.Label += "-rerun"
			r = e.Host.Shell(run)
		}
		lastLog = r.Transcript
		return r.Outcome
	})
	if !res.Green() {
		res.Log = lastLog
	}
	return res
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
		wt := e.Host.Worktree()
		headBefore, _ := wt.HeadSHA()
		e.Log.Event(fmt.Sprintf("CI red — launching auto-fix session %d (cap %d min active)", attempt, int(e.Cfg.ReviewTimeout.Minutes())))
		pctx := e.promptContext(t)
		pctx.CILogs, pctx.CILogAvailable = ciLogs, logAvailable
		res := e.Host.Agent(hostio.AgentRun{
			Label:    fmt.Sprintf("cifix-%d", attempt),
			Model:    e.Cfg.ReviewModel,
			Prompt:   prompt.For(prompt.CIFix, pctx),
			Warnings: prompt.BodyWarnings(pctx),
			Cap:      e.Cfg.ReviewTimeout,
			// A CI fix reproduces a gate; it drives no browser by hand (#248).
			NoMCP:   true,
			WorkDir: wt.Path(),
		})
		if err := sessionError(res.Outcome, attempt); err != nil {
			return err
		}
		// Ground truth, never the agent's say-so: the fix must be committed (clean
		// worktree) or the harness has nothing trustworthy to push.
		if !wt.Clean() {
			return fmt.Errorf("auto-fix session %d left uncommitted changes — not pushing", attempt)
		}
		// The agent may have correctly concluded the red is not a code defect (a
		// cancelled/superseded/flaky run) and committed nothing. Don't force a
		// speculative diff: add an empty commit so the re-push gives CI a fresh HEAD
		// to re-run against; a no-op if a real fix moved HEAD (BEH-561).
		if err := wt.EnsureCIRerunCommit(headBefore); err != nil {
			return fmt.Errorf("auto-fix session %d: re-trigger commit failed: %w", attempt, err)
		}
		return nil
	}
}

// GatedCI is the CI watch's fix callback both loops that watch an open PR hand
// [hostio.CIWatch]: a [Env.CI] session over the failing logs, whose committed fix
// must then pass every gate before the watch may push it — over the branch, and
// over the branch merged with origin/main, which is what CI checks (#259). It
// counts what it did, so its caller can tell a run that launched a session (whose
// transcripts a retrospective mines) or had a fix pushed from one that did
// neither.
type GatedCI struct {
	env   Env
	ciFix func(ciLogs string, logAvailable bool) error
	// Attempts is how many CIFix sessions the watch launched.
	Attempts int
	// Accepted is how many fixes passed the gates; the watch pushes each one.
	Accepted int
	// refusal is why the last fix was refused before the push: the red merge
	// gate's output, which the next attempt works from in place of the CI log the
	// watch hands it. That log is the red the refused fix already answered.
	refusal string
}

// GatedCI returns the gated CI fix over t, for one CI watch.
func (e Env) GatedCI(t Target) *GatedCI { return &GatedCI{env: e, ciFix: e.CI(t)} }

// Fix runs one CIFix session and gates its commit. A gate red only in files the
// fix left alone is re-run once before it refuses the fix (#167); not knowing what
// the fix changed leaves the plain gates. A red gate refuses the fix, so the watch
// pushes nothing. A fix green on the branch is then gated merged with origin/main
// ([GatedCI.mergeGate]); a red there is refused with [ci.ErrFixRefused], and the
// next attempt gets its output as its logs.
func (g *GatedCI) Fix(ciLogs string, logAvailable bool) error {
	g.Attempts++
	if g.refusal != "" {
		ciLogs, logAvailable, g.refusal = g.refusal, true, ""
	}
	wt := g.env.Host.Worktree()
	before, _ := wt.HeadSHA()
	if err := g.ciFix(ciLogs, logAvailable); err != nil {
		return err
	}
	label := fmt.Sprintf("gate-cifix%d", g.Attempts)
	var gate GateResult
	if fixed, err := wt.ChangedSince(before); before != "" && err == nil {
		gate = g.env.GatesOver(label, fixed)
	} else {
		gate = g.env.Gates(label)
	}
	if !gate.Green() {
		return fmt.Errorf("gate %q is red over auto-fix %d (exit %d) — not pushing", gate.FailedGate, g.Attempts, gate.Outcome.ExitCode)
	}
	if err := g.mergeGate(label); err != nil {
		return err
	}
	g.Accepted++
	return nil
}

// mergeGateTailLines bounds the red merge gate's log the next CIFix session gets.
const mergeGateTailLines = 200

// mergeGate runs the gates over the fix merged with origin/main, in a throwaway
// [gitpkg.TrialMerge], when origin/main is not already in the branch: the red
// check the fix answers ran on the PR's merge ref, so a fix green alone and red
// merged would only send CI red again, a CI round and a fix attempt later (#259).
// origin/main is fetched first, as CI's merge ref would be. A merge that
// conflicts, or that git cannot make, refuses the fix outright; a red gate over
// it refuses with [ci.ErrFixRefused] and keeps the gate's output for the next
// attempt. A docs-only branch is not gated here, as on the branch.
func (g *GatedCI) mergeGate(label string) error {
	e := g.env
	if err := e.Host.FetchMain(); err != nil {
		e.Log.Event(e.Who + " … warning: could not refresh origin/main before gating the fix merged with it: " + err.Error())
	}
	wt := e.Host.Worktree()
	if wt.IsRebasedOnto(gitpkg.BaseRef) || wt.DocsOnly(e.Cfg.DocsOnlyExcludedRoots) {
		return nil
	}
	// The gates the branch skipped are skipped merged too, without saying so again.
	gates := e.toRun(wt, false)
	if len(gates) == 0 {
		return nil
	}
	m, err := wt.TrialMerge(gitpkg.BaseRef)
	if err != nil {
		return fmt.Errorf("could not merge auto-fix %d with %s to gate it as CI will (%v) — not pushing", g.Attempts, gitpkg.BaseRef, err)
	}
	defer func() {
		if err := m.Remove(); err != nil {
			e.Log.Event(fmt.Sprintf("%s … warning: could not remove the trial merge at %s: %v", e.Who, m.Path(), err))
		}
	}()
	if len(m.Conflicts) > 0 {
		return fmt.Errorf("auto-fix %d conflicts with %s in %s — not pushing", g.Attempts, gitpkg.BaseRef, strings.Join(m.Conflicts, ", "))
	}
	e.Log.Event(fmt.Sprintf("%s · gating auto-fix %d merged with %s, as CI checks it (#259)", e.Who, g.Attempts, gitpkg.BaseRef))
	if !e.provisionIn(m.Path(), "prep-"+strings.TrimPrefix(label, "gate-")+"-merge") {
		return fmt.Errorf("post_create could not run in the trial merge of auto-fix %d — not pushing", g.Attempts)
	}
	gate := e.gatesIn(m.Path(), gates, label+"-merge", nil)
	if gate.Green() {
		return nil
	}
	e.Log.Event(fmt.Sprintf("%s ✗ gate %q is red over auto-fix %d merged with %s, though green on the branch alone — not pushing; the next attempt works from its output (#259)", e.Who, gate.FailedGate, g.Attempts, gitpkg.BaseRef))
	g.refusal = mergeRefusal(g.Attempts, gate, tailLines(e.gateLog(gate.Log), mergeGateTailLines))
	return fmt.Errorf("gate %q is red over auto-fix %d merged with %s (exit %d), though green on the branch alone — not pushing: %w", gate.FailedGate, g.Attempts, gitpkg.BaseRef, gate.Outcome.ExitCode, ci.ErrFixRefused)
}

// mergeRefusal is what the attempt after a refused fix is handed as its logs: why
// the harness withheld the fix, and the red merge gate's output.
func mergeRefusal(attempt int, gate GateResult, log string) string {
	if strings.TrimSpace(log) == "" {
		log = gate.Outcome.StderrTail
	}
	return fmt.Sprintf("The harness did NOT push auto-fix %d. Its own gate %q passed on the branch alone but went red (exit %d) over the branch merged with origin/main — and CI checks the PR's merge with origin/main, so it would go red the same way. Make the branch pass once merged: `git diff HEAD...origin/main` shows what origin/main changed since the branch left it. Do NOT merge or rebase origin/main into the branch; the harness does that itself. The merge gate's output follows.\n\n%s", attempt, gate.FailedGate, gate.Outcome.ExitCode, log)
}

// tailLines is the last n lines of s.
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
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

// --- cleanup -----------------------------------------------------------------

// Checkpoint saves what a session left uncommitted in the worktree as a local
// recovery checkpoint commit on its branch ([gitpkg.Worktree.Checkpoint]), and
// pushes nothing (#246). The worktree is then clean, so [Env.Cleanup] removes it,
// and a later checkout can start from the checkpoint. A clean worktree, or none,
// is a no-op. It fails when the commit does, leaving the worktree dirty.
func (e Env) Checkpoint(identifier string) error {
	wt := e.Host.Worktree()
	if !wt.Exists() || wt.Clean() {
		return nil
	}
	if err := wt.Checkpoint(identifier, e.Who); err != nil {
		return err
	}
	head, _ := wt.HeadSHA()
	e.Log.Event(fmt.Sprintf("%s ⚑ saved the uncommitted work in %s as the local recovery checkpoint %s on %s — not pushed; the next attempt at this head starts from it", e.Who, wt.Path(), shortSHA(head), wt.Branch()))
	return nil
}

// shortSHA abbreviates a SHA for the run log.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Cleanup removes the worktree once it is clean: the branch on origin is the
// record, and the next run checks it out or cuts it afresh. A worktree left dirty
// is kept for a look, and so is one git will not remove. Both loops that work a
// worktree the ticket pipeline did not create share it.
func (e Env) Cleanup() {
	wt := e.Host.Worktree()
	if !wt.Clean() {
		e.Log.Event(fmt.Sprintf("%s — worktree %s kept: it holds uncommitted changes", e.Who, wt.Path()))
		return
	}
	if err := wt.Remove(); err != nil {
		e.Log.Event(fmt.Sprintf("%s — worktree %s kept: %v", e.Who, wt.Path(), err))
		return
	}
	e.Log.Event(fmt.Sprintf("%s — removed the clean worktree %s", e.Who, wt.Path()))
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
	// BaseRed, when set, marks a red re-gate whose failing Gate fails on
	// origin/main alone too (#250): the base is red, not the branch, so the caller
	// holds the work until main moves rather than count the failure against it.
	BaseRed *BaseRed
}

// BaseRed is origin/main failing, with nothing of the branch on it, the Gate a
// resolved branch's re-gate failed (#250).
type BaseRed struct {
	// SHA is the base, and Verdict the red verdict the record holds for it
	// (ADR-0036), whose expiry ends the hold.
	SHA     string
	Verdict basegate.Verdict
	// Recorded marks a verdict this run gated and recorded, rather than read; the
	// run that records a red base files its finding. Gate is that run's red gate.
	Recorded bool
	Gate     GateResult
}

// baseGateStep labels the containers that gate origin/main alone after a red
// re-gate, and baseConfirmStep their re-run over a red base (#189). They are the
// labels the implementation stage gates an untouched base under.
const (
	baseGateStep    = "basegate"
	baseConfirmStep = "basegate-confirm"
)

// ResolveConflict launches a bounded sandboxed conflict-resolution session over the
// existing worktree (steered by the RebaseFix prompt to bring the branch onto
// origin/main by c.Strategy, resolve, and commit), then enforces ground truth — the
// verdict is never the agent's say-so but verify.RebaseResolution over the
// worktree's git state (session exit, clean tree, origin/main an ancestor of the
// tip). On a clean resolution it re-runs the gates over the rewritten tree and
// only reports OK when they are green and the worktree still clean. A failed
// session may have left the worktree mid-replay, so it is restored to a clean,
// on-branch state for the next resume.
//
// A session can also find nothing to resolve: the branch already sat on origin/main,
// so its replay applied cleanly and left the content exactly as it was (#164). That
// is logged as such, not as a resolved conflict, so the run log does not record a
// resolution that never happened.
//
// What becomes of the worktree is the caller's to say: the review stage keeps it,
// the PR loop removes it once clean (#252).
func (e Env) ResolveConflict(c Conflict) ConflictResult {
	wt := e.Host.Worktree()
	onBase := wt.IsRebasedOnto(gitpkg.BaseRef)
	treeBefore, _ := wt.HeadTree()
	e.Log.Event(fmt.Sprintf("launching conflict-resolution session (%s onto origin/main, cap %d min active)", c.Strategy, int(e.Cfg.ReviewTimeout.Minutes())))
	pctx := e.promptContext(c.Target)
	pctx.Merge = c.Strategy == Merge
	res := e.Host.Agent(hostio.AgentRun{
		Label:    "rebasefix",
		Model:    e.Cfg.ReviewModel,
		Prompt:   prompt.For(prompt.RebaseFix, pctx),
		Warnings: prompt.BodyWarnings(pctx),
		Cap:      e.Cfg.ReviewTimeout,
		WorkDir:  wt.Path(),
	})
	e.Log.Event(fmt.Sprintf("conflict-resolution session exited (code %d)", res.ExitCode))

	verdict := verify.RebaseResolution(wt, res.Outcome)
	if !verdict.OK {
		wt.AbortRebase()
		if verdict.SpendingCapAbort {
			e.Log.Event(e.Who + " ↻ conflict resolution deferred — spending cap reached, retry after reset (BEH-494)")
			return ConflictResult{SpendingCapAbort: true, Reason: verdict.Reason}
		}
		e.Log.Event(e.Who + " ✗ " + verdict.Reason + " — nothing pushed")
		return ConflictResult{Reason: verdict.Reason}
	}
	if treeAfter, _ := wt.HeadTree(); onBase && treeBefore != "" && treeAfter == treeBefore {
		e.Log.Event(e.Who + " ✓ replay was clean in the sandbox — no conflict to resolve; the branch's content is unchanged on origin/main — re-gating before push (#164)")
	} else {
		e.Log.Event(e.Who + " ✓ " + verdict.Reason)
	}

	// The resolution rewrote the tree, so any earlier gate result is stale — re-run
	// the gates before trusting the push. Push only on a green gate over a
	// still-clean worktree.
	gate := c.Regate()
	if !gate.Green() {
		if base := e.redBase(gate); base != nil {
			reason := fmt.Sprintf("post-resolution gate %q is red (exit %d), and origin/main %s fails it too with nothing of the branch on it", gate.FailedGate, gate.Outcome.ExitCode, shortSHA(base.SHA))
			e.Log.Event(e.Who + " ⏸ " + reason + " — base is red, not the branch; nothing pushed (#250)")
			return ConflictResult{Resolved: true, Reason: reason, Gate: gate, BaseRed: base}
		}
	}
	if !gate.Green() || !wt.Clean() {
		reason := fmt.Sprintf("post-resolution gate re-run failed (exit %d)", gate.Outcome.ExitCode)
		e.Log.Event(e.Who + " ✗ " + reason + " — nothing pushed")
		return ConflictResult{Resolved: true, Reason: reason, Gate: gate}
	}
	e.Log.Event(e.Who + " ✓ post-resolution gate re-run is green — clear to push")
	return ConflictResult{OK: true, Resolved: true, Gate: gate}
}

// redBase reports whether origin/main itself fails the Gate the re-gate over a
// resolved branch failed (#250): a base two PRs left red fails the re-gate of
// every branch resolved onto it, through no fault of the branch. The verdict
// record answers for a SHA it already holds (ADR-0036). Otherwise origin/main is
// checked out alone in a [gitpkg.Worktree.TrialBase], provisioned and gated, and a
// red is confirmed by a re-run before it is recorded, as the implementation stage
// confirms one (#189). Nil means the branch keeps the blame: the base passes that
// Gate or fails another, the re-gate's red or the base's was environmental (which
// says nothing about either), or the base could not be read or gated.
func (e Env) redBase(regate GateResult) *BaseRed {
	if regate.FailedGate == "" || regate.Outcome.Retryable() || regate.Outcome.CapKilled {
		return nil
	}
	wt := e.Host.Worktree()
	sha, err := wt.RefSHA(gitpkg.BaseRef)
	if err != nil {
		e.Log.Event(fmt.Sprintf("%s … warning: could not read %s to check whether the base fails gate %q too: %v", e.Who, gitpkg.BaseRef, regate.FailedGate, err))
		return nil
	}
	verdicts := basegate.For(e.Cfg)
	if v, ok := verdicts.Read(sha); ok {
		if v.Gate != regate.FailedGate {
			return nil
		}
		e.Log.Event(fmt.Sprintf("%s · base %s failed gate %q at %s already — not gating it again (#250)", e.Who, shortSHA(sha), v.Gate, v.At.Format(time.RFC3339)))
		return &BaseRed{SHA: sha, Verdict: v}
	}

	e.Log.Event(fmt.Sprintf("%s · gating %s %s alone, to tell a red base from a red branch (#250)", e.Who, gitpkg.BaseRef, shortSHA(sha)))
	b, err := wt.TrialBase(sha)
	if err != nil {
		e.Log.Event(fmt.Sprintf("%s … warning: could not check out %s alone to gate it (%v) — the red re-gate stays the branch's", e.Who, gitpkg.BaseRef, err))
		return nil
	}
	defer func() {
		if err := b.Remove(); err != nil {
			e.Log.Event(fmt.Sprintf("%s … warning: could not remove the trial base at %s: %v", e.Who, b.Path(), err))
		}
	}()
	if !e.provisionIn(b.Path(), "prep-"+baseGateStep) {
		return nil
	}
	res := e.gatesIn(b.Path(), e.Cfg.Gates, baseGateStep, nil)
	if !res.Green() && !environmental(res) {
		e.Log.Event(fmt.Sprintf("%s ↻ base gate %q is red (exit %d) — re-running the gates once before the base counts as red (#189)", e.Who, res.FailedGate, res.Outcome.ExitCode))
		res = e.gatesIn(b.Path(), e.Cfg.Gates, baseConfirmStep, nil)
	}
	if environmental(res) {
		e.Log.Event(fmt.Sprintf("%s ⚠ base gate %q failed environmentally (exit %d) — that says nothing about the base; the red re-gate stays the branch's", e.Who, res.FailedGate, res.Outcome.ExitCode))
		return nil
	}
	v, err := verdicts.Write(sha, basegate.Verdict{Gate: res.FailedGate})
	if err != nil {
		e.Log.Event("⚠ could not record the base verdict (" + err.Error() + ") — the next run on this base gates it again")
	}
	if v.Gate != regate.FailedGate {
		return nil
	}
	return &BaseRed{SHA: sha, Verdict: v, Recorded: true, Gate: res}
}

// environmental reports whether a red gate failed for its environment, not its
// tree: an OOM-kill or a failed launch through its retries, or its cap.
func environmental(g GateResult) bool {
	return !g.Green() && (g.Outcome.Retryable() || g.Outcome.CapKilled)
}
