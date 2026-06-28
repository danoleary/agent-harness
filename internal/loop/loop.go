// Package loop is the autonomous daemon spine: it drives the single-ticket
// pipeline (internal/pipeline) over the ready-for-agent queue in a long-running
// for{}, with graceful between-ticket stop control (DESIGN.md "The loop" +
// "Stop control") and a circuit breaker that winds the daemon down when it is
// repeatedly failing to ship. Like the pipeline, the I/O is injected as a Deps
// struct so the sequencing — startup, select→run→repeat, idle-and-re-poll on an
// empty queue, stop between tickets, trip-and-exit at the breaker threshold — is
// unit-testable without Docker, Linear, or real signals.
//
// It does NOT yet have the spending-cap backoff (release-to-Todo + long sleep);
// that layers on later. The breaker is deliberately blind to a cap abort (it is a
// retry-after-reset control signal, not a ship failure) — see breaker.go.
package loop

import "time"

// TicketOutcome is what the loop learns from running one ticket through the
// pipeline: the breaker keys on these typed signals, NOT on the raw exit code
// (DESIGN.md §Circuit breaker). A ticket that reached a pushed PR is a success even
// if a later stage failed (retrospective-only failure, CI-red-after-budget); a
// spending-cap abort is neither success nor failure — the breaker stays blind to it.
type TicketOutcome struct {
	// ReachedPushedPR is the breaker's success signal: implementation + review +
	// push produced a PR. It resets the consecutive-failure counter.
	ReachedPushedPR bool
	// SpendingCapAbort marks a run an external Anthropic spending cap aborted before
	// it could ship. It is neutral to the breaker (the cap runaway is the backoff's
	// job, not the breaker's) unless a PR already shipped, in which case the PR wins.
	SpendingCapAbort bool
}

// Narrator is the one-line console + run.jsonl narration sink (satisfied by
// *runlog.Logger, as in the pipeline). An interface keeps Run testable.
type Narrator interface {
	Event(string)
}

// Deps are the loop's injectable dependencies. Selection and pipeline execution
// are pre-bound thunks (the caller captures cfg/log/runID); the stop predicate,
// sentinel clear, and main fetch are the host-side I/O the real cmd/loop wires to
// signals + the filesystem.
type Deps struct {
	// ClearStopFile removes any stale STOP sentinel left by a prior run, so a
	// leftover file can't make a fresh daemon stop before doing any work.
	ClearStopFile func() error
	// FetchMain fast-forwards the primary checkout's origin/main, at startup and
	// after every ticket ("pull main after every session").
	FetchMain func() error
	// StopRequested folds the two stop signals — the SIGINT flag and the
	// agent-harness/STOP sentinel — into one predicate, checked at every
	// between-ticket checkpoint and between idle ticks (DESIGN.md "Two signals,
	// one check").
	StopRequested func() bool
	// ResolveNext selects and claims the top-of-queue eligible ticket, returning
	// its identifier and ok=false when the queue is empty.
	ResolveNext func() (identifier string, ok bool)
	// RunPipeline runs the full implementation→review→retrospective pipeline over
	// one ticket and returns the typed outcome the breaker keys on (did it reach a
	// pushed PR?), not the opaque exit code.
	RunPipeline func(identifier string) TicketOutcome
	// Sleep waits for d, broken by the caller into short ticks so a stop landing
	// during an idle wait is observed within one TickInterval, not one PollInterval.
	Sleep func(d time.Duration)
	// PollInterval is how long to idle before re-polling an empty queue.
	PollInterval time.Duration
	// TickInterval is the granularity the idle wait is broken into so stop stays
	// responsive during the idle window.
	TickInterval time.Duration
	// MaxConsecutiveFailures is the circuit-breaker threshold: after this many
	// consecutive tickets fail to reach a pushed PR, the daemon trips and winds down
	// (DESIGN.md §Circuit breaker). A non-positive value disables the breaker.
	MaxConsecutiveFailures int
	// Log is the narration sink.
	Log Narrator
}

// Run is the daemon spine. Startup clears the stale STOP sentinel and fetches
// main; then it loops: stop-check → select → (idle+re-poll if empty | run+fetch
// if a ticket) — winding down cleanly between tickets on a stop request. It
// returns the process exit code: 0 on a deliberate stop or empty-then-stop, the
// daemon never failing merely because the queue ran dry.
func Run(d Deps) int {
	// Startup: clear any stale STOP left by a prior run BEFORE the first stop-check,
	// so a leftover sentinel can't abort a fresh daemon before it does any work.
	if err := d.ClearStopFile(); err != nil {
		d.Log.Event("loop … warning: could not clear stale STOP sentinel: " + err.Error())
	}
	if err := d.FetchMain(); err != nil {
		d.Log.Event("loop … warning: could not fast-forward origin/main at startup: " + err.Error())
	}

	b := newBreaker(d.MaxConsecutiveFailures)
	for {
		// Stop only ever lands BETWEEN tickets — a graceful, per-ticket checkpoint
		// (DESIGN.md "Stop control"): stopping mid-ticket would strand a worktree.
		if d.StopRequested() {
			d.Log.Event("loop — stop requested; winding down")
			return 0
		}

		identifier, ok := d.ResolveNext()
		if !ok {
			// The daemon difference from `pipeline --next`: an empty queue is a
			// normal steady state, so the loop idles for PollInterval and re-polls
			// rather than exiting (DESIGN.md "The loop"). The idle is broken into
			// short ticks that re-check stop, so a SIGINT/sentinel landing mid-idle
			// is honoured within one TickInterval, not a whole poll interval later.
			d.Log.Event("loop — queue empty; idling before re-poll")
			d.idleWait()
			continue
		}
		// Run the full pipeline over the claimed ticket, then fetch main again —
		// "pull main after every session" keeps the next ticket's merge-base honest
		// across the loop's long run.
		outcome := d.RunPipeline(identifier)
		if err := d.FetchMain(); err != nil {
			d.Log.Event("loop … warning: could not fast-forward origin/main after ticket: " + err.Error())
		}

		// Fold the ticket into the breaker. The success signal is "did it reach a
		// pushed PR?" — not the pipeline exit code — so a hard-but-shipped ticket
		// (retrospective failed, CI red after budget) resets the counter, while a
		// ticket that never produced a PR increments it (DESIGN.md §Circuit breaker).
		b.record(identifier, outcome)
		if b.tripped() {
			// Trip on the SAME graceful wind-down path as a STOP (exit, not pause): it
			// forces a human to investigate before more tickets are consumed, since 3
			// identical failures are almost always environmental (expired auth, broken
			// base build) that would sink the next ticket too.
			d.Log.Event("loop ✗ " + b.report() + " — winding down; fix the environment and relaunch")
			return 0
		}
	}
}

// idleWait waits out one PollInterval before the loop re-polls an empty queue,
// but breaks the wait into TickInterval chunks and re-checks StopRequested
// between each chunk. A stop (SIGINT/sentinel) landing mid-idle is therefore
// observed within one TickInterval, not a whole PollInterval later — the
// responsiveness the daemon's "idle is an interruptible stop point" (ADR-0004)
// depends on. A non-positive TickInterval degrades to a single PollInterval
// sleep so a misconfigured tick can't spin.
func (d Deps) idleWait() {
	if d.TickInterval <= 0 {
		d.Sleep(d.PollInterval)
		return
	}
	for waited := time.Duration(0); waited < d.PollInterval; waited += d.TickInterval {
		if d.StopRequested() {
			return
		}
		d.Sleep(d.TickInterval)
	}
}
