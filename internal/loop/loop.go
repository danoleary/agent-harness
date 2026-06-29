// Package loop is the autonomous daemon spine: it drives the single-ticket
// pipeline (internal/pipeline) over the ready-for-agent queue in a long-running
// for{}, with graceful between-ticket stop control (DESIGN.md "The loop" +
// "Stop control") and a circuit breaker that winds the daemon down when it is
// repeatedly failing to ship. Like the pipeline, the I/O is injected as a Deps
// struct so the sequencing — startup, select→run→repeat, idle-and-re-poll on an
// empty queue, stop between tickets, trip-and-exit at the breaker threshold — is
// unit-testable without Docker, Linear, or real signals.
//
// A spending-cap abort is handled as a control-flow signal, not a ticket verdict:
// the run's ticket is released back to Todo and the daemon enters a long
// interruptible backoff before re-polling (auto-resuming once the cap window
// resets) — see Run. The breaker is deliberately blind to a cap abort (it is a
// retry-after-reset control signal, not a ship failure) — see breaker.go.
package loop

import (
	"fmt"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

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
	// RecommendClose marks the BEH-603 no-op disposition: the review stage found the
	// branch makes zero net change against origin/main and correctly concluded the
	// ticket should be closed as a duplicate/superseded rather than opened as an
	// empty-commit PR. It is a correct terminal outcome — neutral to the breaker (not
	// a ship failure) — and the loop keeps the ticket In Progress for a human to close
	// rather than releasing it to Todo, so it never re-loops into the same conclusion.
	RecommendClose bool
}

// Narrator is the one-line console + run.jsonl narration sink (satisfied by
// *runlog.Logger, as in the pipeline). Structured additionally feeds the global
// loop.jsonl the viewer tails (ADR-0005). An interface keeps Run testable.
type Narrator interface {
	Event(string)
	Structured(loopstream.Record)
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
	// ReleaseTicket returns a claimed ticket to Todo after a run made no progress —
	// a spending-cap abort (DESIGN.md §Spending-cap abort backoff) or any other no-PR
	// run (BEH-590) — so it isn't stranded In Progress. A release failure is narrated,
	// not fatal — the daemon still backs off / continues and re-polls.
	ReleaseTicket func(identifier string) error
	// CommentTicket posts a breadcrumb on the released ticket noting the run died and
	// why (BEH-590), so a repeatedly-failing ticket is visible on the board rather
	// than silently bouncing Todo↔In Progress. Best-effort: a nil func or a post
	// failure is tolerated (the release is what matters; the comment is the breadcrumb).
	CommentTicket func(identifier, body string) error
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
	// CapBackoffHeartbeat is how often the cap-abort backoff emits a "still capped,
	// re-poll ~HH:MMZ" heartbeat so a long (default 45m) backoff stays observable
	// rather than a black hole (BEH-605). It is coarser than TickInterval to avoid
	// flooding the log: the wait still ticks at TickInterval for stop-responsiveness,
	// but only narrates a heartbeat once this much wall-clock has accrued since the
	// last one. A non-positive value emits no intermediate heartbeats — only the entry
	// and wake records bookend the wait.
	CapBackoffHeartbeat time.Duration
	// CapBackoff is how long the daemon sleeps after a spending-cap abort before
	// re-polling — long enough to let the external cap window reset, auto-resuming
	// once it does (DESIGN.md §Spending-cap abort backoff). Broken into the same
	// TickInterval chunks as the idle wait, so a stop landing mid-backoff is honoured
	// within one tick rather than ~45 minutes later.
	CapBackoff time.Duration
	// DiskReclaimThreshold is the soft free-disk floor (bytes) below which the loop
	// proactively reclaims host disk between tickets, BEFORE the hard 5 GiB sandbox
	// preflight floor would refuse a launch (ADR-0005). Zero disables reclaim entirely
	// — the loop then makes no statfs/prune calls at all.
	DiskReclaimThreshold uint64
	// FreeDisk reports the bytes available on the worktrees volume via a cheap statfs;
	// it gates the reclaim step so a healthy disk triggers no shell-out / gh calls. A
	// nil FreeDisk (or a statfs error) disables reclaim defensively — a disk check
	// must never block the daemon.
	FreeDisk func() (uint64, error)
	// PruneMergedWorktrees shells out to scripts/prune-merged-worktrees.sh --yes — the
	// existing, tested, squash-merge-aware reclaimer that removes only worktrees whose
	// PR has merged and whose tree is clean — and returns how many it removed. The
	// merged/clean classification lives entirely in the script (single source of
	// truth); the loop never reimplements it. A failure is non-fatal (ADR-0005).
	PruneMergedWorktrees func() (removed int, err error)
	// StorePrune runs `pnpm store prune` — a cheap, non-destructive, network-free
	// secondary reclaim — only when free disk is STILL below the threshold after the
	// worktree prune. A nil StorePrune skips the secondary step; a failure is non-fatal.
	StorePrune func() error
	// MaxConsecutiveFailures is the circuit-breaker threshold: after this many
	// consecutive tickets fail to reach a pushed PR, the daemon trips and winds down
	// (DESIGN.md §Circuit breaker). A non-positive value disables the breaker.
	MaxConsecutiveFailures int
	// MaxTickets is the optional ceiling on *attempted* tickets: once this many
	// tickets have been run, the daemon winds down on the clean stop path (a
	// deliberate stop, not a failure) — the AFK safety valve. Zero or negative means
	// unlimited (DESIGN.md §cmd/loop config knobs).
	MaxTickets int
	// MaxRuntime is the optional wall-clock ceiling: once the daemon has been running
	// this long, it winds down on the clean stop path at the next between-ticket
	// checkpoint. Zero or negative means unlimited (DESIGN.md §cmd/loop config knobs).
	MaxRuntime time.Duration
	// Now reports the current time for the MaxRuntime ceiling; injected so the ceiling
	// is testable without real time. A nil Now defaults to time.Now.
	Now func() time.Time
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

	now := d.Now
	if now == nil {
		now = time.Now
	}
	start := now()

	b := newBreaker(d.MaxConsecutiveFailures)
	attempted := 0
	for {
		// Stop only ever lands BETWEEN tickets — a graceful, per-ticket checkpoint
		// (DESIGN.md "Stop control"): stopping mid-ticket would strand a worktree.
		if d.StopRequested() {
			d.Log.Event("loop — stop requested; winding down")
			return 0
		}

		// Optional AFK wall-clock ceiling: once the daemon has run for MaxRuntime, wind
		// down on the same clean stop path between tickets — a deliberate stop, not a
		// failure (DESIGN.md §cmd/loop config knobs). Zero/negative = unlimited.
		if d.MaxRuntime > 0 && now().Sub(start) >= d.MaxRuntime {
			d.Log.Event("loop — max runtime reached; winding down")
			return 0
		}

		// Optional AFK ceiling: once MaxTickets tickets have been attempted, wind down
		// on the same clean stop path as STOP — a deliberate stop, not a failure
		// (DESIGN.md §cmd/loop config knobs). Checked here, between tickets, so the
		// (N+1)th ticket is never even selected. Zero/negative = unlimited.
		if d.MaxTickets > 0 && attempted >= d.MaxTickets {
			d.Log.Event("loop — max tickets reached; winding down")
			return 0
		}

		// Reclaim disk between tickets, before selecting the next one, when no sandbox
		// is active (ADR-0005). Gated on a cheap statfs so a healthy disk costs nothing;
		// a failed prune is narrated and swallowed — reclaim is never a ticket outcome
		// and never touches the breaker.
		d.reclaimDisk()

		identifier, ok := d.ResolveNext()
		if !ok {
			// The daemon difference from `pipeline --next`: an empty queue is a
			// normal steady state, so the loop idles for PollInterval and re-polls
			// rather than exiting (DESIGN.md "The loop"). The idle is broken into
			// short ticks that re-check stop, so a SIGINT/sentinel landing mid-idle
			// is honoured within one TickInterval, not a whole poll interval later.
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindIdle, Message: "loop — queue empty; idling before re-poll"})
			d.idleWait()
			continue
		}
		// Run the full pipeline over the claimed ticket. Count it as attempted before
		// any outcome branching — a cap-aborted run was still an attempt, so it counts
		// toward the MaxTickets ceiling like any other.
		outcome := d.RunPipeline(identifier)
		attempted++

		// A spending-cap abort is a control-flow signal, NOT a ticket verdict
		// (DESIGN.md §Spending-cap abort backoff): an external Anthropic cap aborted
		// the run before it could ship, so no progress was made and it is not the
		// diff's fault. Release the ticket back to Todo (don't strand it In Progress),
		// leave the breaker untouched (it stays blind to the cap runaway by design),
		// and enter a long interruptible backoff before re-polling — auto-resuming once
		// the cap window resets. A shipped PR wins: if the run still reached a pushed
		// PR, it's a success, so fall through to the normal after-ticket fold.
		if outcome.SpendingCapAbort && !outcome.ReachedPushedPR {
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindCapAbort, Ticket: identifier, Message: "loop — spending-cap abort on " + identifier + "; releasing to Todo"})
			if err := d.ReleaseTicket(identifier); err != nil {
				d.Log.Event("loop … warning: could not release " + identifier + " to Todo after cap abort: " + err.Error())
			}
			d.comment(identifier, "Autonomous run aborted by the Anthropic spending cap before it could ship — released back to Todo. The daemon backs off and auto-resumes once the cap window resets, so this should retry on its own.")
			d.capBackoffWait(identifier)
			continue
		}

		// Fetch main again — "pull main after every session" keeps the next ticket's
		// merge-base honest across the loop's long run.
		if err := d.FetchMain(); err != nil {
			d.Log.Event("loop … warning: could not fast-forward origin/main after ticket: " + err.Error())
		}

		// Recommend-close (BEH-603): the run concluded the branch makes zero net change
		// against origin/main — a duplicate/superseded ticket the review stage correctly
		// declined to ship as an empty-commit PR. Unlike every other no-PR mode this one
		// must NOT release back to Todo: releasing would let the dispatch guard re-grab it
		// and re-run the whole pipeline to the same "nothing to ship" conclusion forever.
		// Keep it In Progress for a human to close as a duplicate, and leave a breadcrumb
		// recommending exactly that (distinct from the generic "released to Todo" note,
		// which would mislead). It is breaker-neutral (handled in b.record), so it falls
		// through to the normal after-ticket fold below.
		switch {
		case outcome.RecommendClose:
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindRecommendClose, Ticket: identifier, Message: "loop — " + identifier + " makes no net change against main; keeping it In Progress for a human to close as a duplicate/superseded (BEH-603)"})
			d.comment(identifier, "Autonomous run found this branch makes zero net change against `main` (an empty diff) — there is nothing to ship. The substantively correct outcome is to close this ticket as a duplicate/superseded rather than ship an empty-commit change; if a PR was already opened (the branch only became a no-op after the pre-push rebase) it is a no-op and should be closed too. Kept In Progress for a human to close; it was deliberately NOT released to Todo so it won't be re-picked and re-run to the same conclusion. See the run logs for the cause.")
		case !outcome.ReachedPushedPR:
			// Release-on-no-PR (BEH-590): a run that finished WITHOUT opening a pushed PR —
			// any non-cap failure mode: OOM, sandbox crash, a review stage that died before
			// pushing, an empty-diff verification failure — left the ticket claimed In
			// Progress with nothing to show for it. The dispatch claimed it on select, so
			// unless we undo the claim the board reads as "in flight" forever and the
			// dispatch guard never re-grabs it. The cap-abort branch above already released
			// (and continued), so this only fires for the non-cap no-PR case. A shipped PR
			// (ReachedPushedPR) is the success signal — never released, even on a non-zero
			// exit (CI red after the auto-fix budget). Best-effort: a Linear hiccup here is
			// warned, never fatal — the daemon must keep running.
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: identifier, Message: "loop — " + identifier + " produced no PR; releasing back to Todo so it isn't stranded In Progress (BEH-590)"})
			if err := d.ReleaseTicket(identifier); err != nil {
				d.Log.Event("loop … warning: could not release " + identifier + " to Todo after a no-PR run: " + err.Error())
			}
			d.comment(identifier, "Autonomous run finished with no PR for this ticket — released back to Todo so a later run can re-grab it. Repeated occurrences here mean the ticket keeps failing to ship; see the run logs for the cause.")
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
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindBreakerTrip, Ticket: identifier, Message: "loop ✗ " + b.report() + " — winding down; fix the environment and relaunch"})
			return 0
		}
	}
}

// comment posts a best-effort breadcrumb on a released ticket (BEH-590). It is a
// no-op when no CommentTicket is wired, and a post failure is narrated rather than
// fatal — the release already happened, and a missing breadcrumb must never sink
// the daemon (mirrors AddComment's best-effort contract).
func (d Deps) comment(identifier, body string) {
	if d.CommentTicket == nil {
		return
	}
	if err := d.CommentTicket(identifier, body); err != nil {
		d.Log.Event("loop … warning: could not comment on " + identifier + " after release: " + err.Error())
	}
}

// reclaimDisk proactively frees host disk between tickets when free space has
// fallen below the soft DiskReclaimThreshold, BEFORE the hard MinFreeDiskBytes
// sandbox floor would refuse a launch (ADR-0005). The named goal is dead worktrees,
// so the squash-merge-aware prune script runs first; if free space is still below
// the threshold afterwards, `pnpm store prune` is a cheap, non-destructive follow-up.
//
// Three invariants, all from ADR-0005:
//   - Gated on a cheap statfs: a healthy disk (or a disabled threshold / missing
//     statfs) makes no prune / gh / shell calls at all.
//   - Never a ticket outcome: a failed statfs/prune is narrated and swallowed; the
//     loop continues and the circuit breaker is never touched (the breaker keys only
//     on "did the ticket reach a pushed PR?", ADR-0004).
//   - Narrate only when it acts: a "reclaimed N worktree(s)" line is emitted only
//     when the prune actually removed something; a healthy or no-op iteration is silent.
func (d Deps) reclaimDisk() {
	if d.DiskReclaimThreshold == 0 || d.FreeDisk == nil {
		return // reclaim disabled
	}
	before, err := d.FreeDisk()
	if err != nil {
		// A statfs error must never block the loop — degrade to a warning and carry on,
		// the same way the sandbox preflight treats an unreadable statfs as non-blocking.
		d.Log.Event("loop … warning: could not statfs worktrees volume for disk reclaim (skipping): " + err.Error())
		return
	}
	if before >= d.DiskReclaimThreshold {
		return // disk healthy — silent, no shell-out
	}

	removed, perr := d.PruneMergedWorktrees()
	if perr != nil {
		d.Log.Event("loop … warning: pruning merged worktrees failed during disk reclaim (continuing): " + perr.Error())
	}

	// Re-statfs: only run the cheap secondary store prune if still below the threshold
	// after the worktree prune. Track the latest reading so the narration can report
	// the bytes freed.
	after := before
	if mid, merr := d.FreeDisk(); merr == nil {
		after = mid
		if mid < d.DiskReclaimThreshold && d.StorePrune != nil {
			if serr := d.StorePrune(); serr != nil {
				d.Log.Event("loop … warning: pnpm store prune failed during disk reclaim (continuing): " + serr.Error())
			} else if final, ferr := d.FreeDisk(); ferr == nil {
				after = final
			}
		}
	}

	// Narrate only when the prune actually removed something (ADR-0005).
	if removed > 0 {
		msg := fmt.Sprintf("loop — reclaimed %d merged worktree(s) under disk pressure", removed)
		if after > before {
			msg += fmt.Sprintf("; freed %d MiB", (after-before)>>20)
		}
		d.Log.Event(msg)
	}
}

// idleWait waits out one PollInterval before the loop re-polls an empty queue,
// interruptibly — see interruptibleSleep. The cap-abort backoff (capBackoffWait)
// uses the same tick-and-re-check-stop mechanism with CapBackoff, additionally
// narrating its progress, so a stop is honoured within one tick during either wait
// (DESIGN.md §The loop, §Spending-cap abort backoff).
func (d Deps) idleWait() { d.interruptibleSleep(d.PollInterval) }

// clock returns the injected Now (for the cap-backoff wake-time narration), falling
// back to time.Now when none was wired — mirroring Run's own nil-Now default so the
// daemon and its helpers agree on the clock.
func (d Deps) clock() func() time.Time {
	if d.Now == nil {
		return time.Now
	}
	return d.Now
}

// capBackoffWait sleeps out the post-cap-abort backoff like interruptibleSleep, but
// narrates it so a long (default 45m) backoff is observable instead of a black hole
// (BEH-605). It bookends the wait with structured KindCapBackoff records — an entry
// naming the duration and expected wake time, and a wake record on natural elapse —
// and emits a periodic "still capped" heartbeat (every CapBackoffHeartbeat) in
// between, so a watcher can see the daemon is alive and counting down. The wait still
// ticks at TickInterval and re-checks StopRequested between ticks, so a stop landing
// mid-backoff is honoured within one tick (the wake record is suppressed on stop —
// the top-of-loop wind-down narration takes over). A non-positive TickInterval
// degrades to a single silent total sleep, like interruptibleSleep.
func (d Deps) capBackoffWait(identifier string) {
	wake := d.clock()().Add(d.CapBackoff)
	wakeStr := wake.UTC().Format("15:04Z")
	d.Log.Structured(loopstream.Record{
		Kind:    loopstream.KindCapBackoff,
		Ticket:  identifier,
		Message: fmt.Sprintf("loop — capped; backing off %s, next re-poll ~%s", d.CapBackoff, wakeStr),
	})

	if d.TickInterval <= 0 {
		d.Sleep(d.CapBackoff)
		d.Log.Structured(loopstream.Record{Kind: loopstream.KindCapBackoff, Ticket: identifier, Message: "loop — cap backoff elapsed; re-polling for " + identifier})
		return
	}

	var sinceBeat time.Duration
	for waited := time.Duration(0); waited < d.CapBackoff; waited += d.TickInterval {
		if d.StopRequested() {
			return // stop wins; the top-of-loop wind-down narrates the exit, not a wake record
		}
		d.Sleep(d.TickInterval)
		sinceBeat += d.TickInterval
		// Heartbeat only between ticks, never on the final one (it coincides with the
		// wake record below), so the bookends never double up.
		if d.CapBackoffHeartbeat > 0 && sinceBeat >= d.CapBackoffHeartbeat && waited+d.TickInterval < d.CapBackoff {
			remaining := d.CapBackoff - (waited + d.TickInterval)
			d.Log.Structured(loopstream.Record{
				Kind:    loopstream.KindCapBackoff,
				Ticket:  identifier,
				Message: fmt.Sprintf("loop — still capped; ~%s remaining, re-poll ~%s", remaining, wakeStr),
			})
			sinceBeat = 0
		}
	}
	d.Log.Structured(loopstream.Record{Kind: loopstream.KindCapBackoff, Ticket: identifier, Message: "loop — cap backoff elapsed; re-polling for " + identifier})
}

// interruptibleSleep waits out total, but breaks the wait into TickInterval chunks
// and re-checks StopRequested between each chunk. A stop (SIGINT/sentinel) landing
// mid-wait is therefore observed within one TickInterval, not a whole total later —
// the responsiveness the daemon's "idle/backoff is an interruptible stop point"
// (ADR-0004) depends on. A non-positive TickInterval degrades to a single total
// sleep so a misconfigured tick can't spin.
func (d Deps) interruptibleSleep(total time.Duration) {
	if d.TickInterval <= 0 {
		d.Sleep(total)
		return
	}
	for waited := time.Duration(0); waited < total; waited += d.TickInterval {
		if d.StopRequested() {
			return
		}
		d.Sleep(d.TickInterval)
	}
}
