// Package loop is the autonomous daemon spine: it drives a [Pipeline] over the
// items a [Source] yields — today the ticket Stages over the tracker's
// ready-for-agent queue — in a long-running for{}, with graceful between-item stop
// control (DESIGN.md "The loop" + "Stop control") and a circuit breaker that winds
// the daemon down when it is repeatedly failing to ship. A loop is a Source plus a
// Pipeline (ADR-0019); the daemon's own chores that leave the process are injected
// as one [Host] (with the operator knobs beside it in [Limits] and the time seam in
// [Clock]), so the sequencing — startup, select→run→repeat, idle-and-re-poll on an
// empty queue, stop between items, trip-and-exit at the breaker threshold — is
// unit-testable without Docker, a tracker, or real signals.
//
// A spending-cap abort is handled as a control-flow signal, not an item verdict:
// the run's item is released back to its Source and the daemon enters a long
// interruptible backoff before re-polling (auto-resuming once the cap window
// resets) — see Run. The breaker is deliberately blind to a cap abort (it is a
// retry-after-reset control signal, not a ship failure) — see breaker.go.
package loop

import (
	"fmt"
	"time"

	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/stages"
)

// StaleClaim is one agent-held claim on a [Source] item that the between-item
// reaper evaluates (BEH-677): when it was claimed (StartedAt) and whether a PR is
// linked. A claim older than ClaimTTL with no linked PR and no remote branch was
// abandoned by a dead agent (sandbox OOM/timeout/crash before pushing) and is
// released back to its Source so it stops occupying the lane. The grace TTL is what
// keeps a healthy agent mid-work (which looks identical — no branch/PR yet) from
// being reaped.
type StaleClaim struct {
	Key         string
	StartedAt   time.Time
	HasLinkedPR bool
}

// Narrator is the one-line console + run.jsonl narration sink (satisfied by
// *runlog.Logger, as in the pipeline). Structured additionally feeds the global
// loop.jsonl the viewer tails (ADR-0005). An interface keeps Run testable.
type Narrator interface {
	Event(string)
	Structured(loopstream.Record)
}

// Clock is the daemon's time seam: the wall clock the MaxRuntime ceiling and the
// cap-backoff wake-time narration read, and the sleep every interruptible wait
// ticks through. One seam rather than two thunks, because a test that fakes one
// without the other is testing a daemon whose clock and sleep disagree.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// SystemClock is the production [Clock]: real time, real sleeping. It is also the
// default an unset Deps.Clock degrades to, so a caller that forgets it gets a
// working daemon rather than a nil dereference.
type SystemClock struct{}

func (SystemClock) Now() time.Time        { return time.Now() }
func (SystemClock) Sleep(d time.Duration) { time.Sleep(d) }

// WorkItem is one unit of work a [Source] hands the daemon: a tracker ticket
// today, an open PR tomorrow (ADR-0019). The daemon reads only Key — to narrate,
// comment and count the item in the breaker — and passes the whole item through to
// the [Pipeline], so what a Source means by an item stays the Source's business.
type WorkItem struct {
	// Key names the item: the tracker Key for a ticket. It is what the lease is on,
	// what the breaker reports, and what every narration and breadcrumb names.
	Key string
	// Branch is the branch the Pipeline will push for the item: for a ticket, the
	// slugged feature branch `<branch_prefix>/<slug>` the Stages create.
	Branch string
	// Context is the Source's own description of the item, which only its Pipeline
	// reads: nil for a ticket (the Stages fetch it by Key), the PR and its failure
	// kind for the PR loop. The daemon never looks inside it.
	Context any
}

// Source is where the daemon's work comes from and where its claims are kept: the
// queue it polls, the claims the reaper sweeps, and the breadcrumbs it leaves on an
// item it gives back (ADR-0019). The ticket Source is the tracker's ready queue
// (loophost.TicketSource); the daemon never learns which tracker, or that it is a
// tracker at all.
type Source interface {
	// Next selects and claims the next eligible item, returning it with the run's
	// held lease on it and ok=false when there is nothing to do. The loop settles
	// the run through that lease — release, close, or keep — so an item a Pipeline
	// already released is never released twice (#25). A non-nil err means the
	// source could not be read or the claim failed: NOT an empty queue, and
	// narrated as such, though the daemon still idles and re-polls.
	Next() (item WorkItem, claim *lease.Lease, ok bool, err error)
	// Claims returns the items this source holds claimed that the reaper evaluates
	// each pass (BEH-677). A list error is warned and swallowed (best-effort, never
	// fatal): the reaper is host-side cleanup, never an item's outcome. Reaping is
	// switched off by a non-positive [Limits.ClaimTTL], not by the absence of an
	// implementation.
	Claims() ([]StaleClaim, error)
	// HasRemoteBranch reports whether a branch was pushed for the item — the second
	// "has work in flight" signal (alongside StaleClaim.HasLinkedPR) that spares a
	// claim from reaping. It must fail SAFE toward true (don't reap on a flaky read).
	HasRemoteBranch(key string) bool
	// Release gives a stale claim back for the reaper (BEH-677): an item some
	// earlier, dead run claimed, which this daemon holds no lease on. A release
	// failure is narrated, not fatal.
	Release(key string) error
	// Comment posts a breadcrumb on an item the run gave back, noting the run died
	// and why (BEH-590), so an item that keeps failing is visible where its owner
	// looks rather than silently bouncing. Best-effort: a post failure is tolerated
	// (the release is what matters; the comment is the breadcrumb).
	Comment(key, body string) error
}

// Pipeline is what happens to one [WorkItem]: for a ticket, the
// implementation→review→retrospective Stages (loophost.TicketPipeline). The loop
// branches only on the Disposition it returns (ADR-0019).
type Pipeline interface {
	// Run works one claimed item and returns the run's folded Result — the loop
	// branches on its Disposition (did it ship? was it aborted?), never on an
	// opaque exit code.
	Run(item WorkItem, claim *lease.Lease) stages.Result
	// Recover attempts to FINISH a no-PR run whose fix was already committed on its
	// branch but never pushed/PR'd — the recovered-checkpoint / verify-only state
	// where the review stage left a gate-green, clean commit yet never reached a PR
	// (BEH-713, the BEH-649 infinite Todo↔In-Progress bounce). It returns the
	// completion outcome (a Shipped Disposition is the success signal) and
	// attempted=true ONLY when there was such a committed-but-no-PR state to finish;
	// attempted=false means there is nothing to recover (a genuine OOM/crash/empty-
	// diff no-PR run), so the loop falls through to the normal release. Host-side
	// (push + open PR), never the loop reimplementing the review machinery; a
	// completion failure just falls through to the release, so a flaky push can't
	// strand the item claimed.
	Recover(item WorkItem) (outcome stages.Result, attempted bool)
}

// Host is the daemon's own chores, the ones no [Source] or [Pipeline] owns: the
// stop-signal filesystem, the startup credential check, keeping main fresh, and the
// host disk it reclaims between items. It is the loop's half of the port ADR-0013
// drew for the Stages — same shape, wider lifetime: a Stage's host is bound to one
// run of one ticket, the daemon's outlives every item it works.
//
// It is one interface rather than a field per operation because a field per
// operation is a seam per operation, each with one real adapter — and because a
// missing field then reads as a missing feature. A method always exists here;
// what an operator can turn off is declared in [Limits], where they can see it
// (ADR-0014).
type Host interface {
	// ClearStopFile removes any stale STOP sentinel left by a prior run, so a
	// leftover file can't make a fresh daemon stop before doing any work.
	ClearStopFile() error
	// CheckAccess proves, once at startup, that the daemon's credentials work: its
	// Source answers a read, and GitHub accepts GH_TOKEN. A failure is fatal — the
	// operator is at the terminal when the daemon starts, which is the cheapest
	// moment to learn a token is dead; discovered later it reads as an idle queue.
	CheckAccess() error
	// StopRequested folds the two stop signals — the SIGINT flag and the
	// .agent-harness/STOP sentinel — into one predicate, checked at every
	// between-item checkpoint and between idle ticks (DESIGN.md "Two signals,
	// one check").
	StopRequested() bool
	// FetchMain fast-forwards the primary checkout's origin/main, at startup and
	// after every item ("pull main after every session").
	FetchMain() error

	// FreeDisk reports the bytes available on the worktrees volume via a cheap statfs;
	// it gates the reclaim step so a healthy disk triggers no shell-out / gh calls. A
	// statfs error disables reclaim defensively — a disk check must never block the
	// daemon.
	FreeDisk() (uint64, error)
	// PruneMergedWorktrees removes the worktrees whose PR has merged and whose tree is
	// clean, and returns how many it removed. The merged/clean classification lives in
	// the Consumer-side prune script (single source of truth); the loop never
	// reimplements it. A failure is non-fatal (ADR-0027).
	PruneMergedWorktrees() (removed int, err error)
	// CachePrune runs the Consumer's declared cache-reclaim command (`cache.prune_command`
	// — `pnpm store prune`, `go clean -modcache`, …): a cheap, non-destructive,
	// network-free secondary reclaim, run only when free disk is STILL below the
	// threshold after the worktree prune. A Consumer that declares no prune command
	// makes it a no-op; a failure is non-fatal.
	CachePrune() error
	// DockerPrune reclaims Docker build cache + unreferenced images — usually the
	// harness's biggest disk hog (it runs `docker run --rm` sandboxes, so stale build
	// cache and dangling images accrete on the Docker volume) and the one the worktree
	// / cache prunes never touch. Run as the LAST reclaim step, only when the cheaper
	// prunes left free disk still below the threshold. A failure is non-fatal
	// (ADR-0027). The removed image self-heals: Preflight rebuilds or pulls the
	// sandbox image on the next launch (resolve-on-miss).
	DockerPrune() error
}

// Limits are the daemon's operator-facing knobs: the cadences it waits on, the
// ceilings it winds down at, and the thresholds that switch the two optional
// between-item chores on. They are values, not behaviour — which is the point.
// Every "is this feature on?" question the daemon answers is answered here, by a
// number an operator sets (DESIGN.md §cmd/loop config knobs), never by whether
// some func happened to be wired at the composition root.
type Limits struct {
	// PollInterval is how long to idle before re-polling an empty queue.
	PollInterval time.Duration
	// TickInterval is the granularity the idle wait is broken into so stop stays
	// responsive during the idle window.
	TickInterval time.Duration
	// CapBackoff is how long the daemon sleeps after a spending-cap abort before
	// re-polling — long enough to let the external cap window reset, auto-resuming
	// once it does (DESIGN.md §Spending-cap abort backoff). Broken into the same
	// TickInterval chunks as the idle wait, so a stop landing mid-backoff is honoured
	// within one tick rather than ~45 minutes later.
	CapBackoff time.Duration
	// CapBackoffHeartbeat is how often the cap-abort backoff emits a "still capped,
	// re-poll ~HH:MMZ" heartbeat so a long (default 45m) backoff stays observable
	// rather than a black hole (BEH-605). It is coarser than TickInterval to avoid
	// flooding the log: the wait still ticks at TickInterval for stop-responsiveness,
	// but only narrates a heartbeat once this much wall-clock has accrued since the
	// last one. A non-positive value emits no intermediate heartbeats — only the entry
	// and wake records bookend the wait.
	CapBackoffHeartbeat time.Duration
	// ClaimTTL is the grace period past a claim's StartedAt before a no-branch/no-PR
	// claim is reaped back to its Source. It must comfortably clear the claim→first-push
	// window so a healthy mid-work agent is never mistaken for a dead one. A
	// non-positive value disables reaping — the loop makes no reaper query at all.
	ClaimTTL time.Duration
	// DiskReclaimThreshold is the soft free-disk floor (bytes) below which the loop
	// proactively reclaims host disk between items, BEFORE the hard 5 GiB sandbox
	// preflight floor would refuse a launch (ADR-0027). Zero disables reclaim entirely
	// — the loop then makes no statfs/prune calls at all.
	DiskReclaimThreshold uint64
	// MaxConsecutiveFailures is the circuit-breaker threshold: after this many
	// consecutive items fail to reach a pushed PR, the daemon trips and winds down
	// (DESIGN.md §Circuit breaker). A non-positive value disables the breaker.
	MaxConsecutiveFailures int
	// MaxTickets is the optional ceiling on *attempted* items (the knob keeps its
	// ticket-loop name, `max_tickets`): once this many items have been run, the daemon winds down on the clean stop path (a
	// deliberate stop, not a failure) — the AFK safety valve. Zero or negative means
	// unlimited (DESIGN.md §cmd/loop config knobs).
	MaxTickets int
	// MaxRuntime is the optional wall-clock ceiling: once the daemon has been running
	// this long, it winds down on the clean stop path at the next between-item
	// checkpoint. Zero or negative means unlimited (DESIGN.md §cmd/loop config knobs).
	MaxRuntime time.Duration
}

// Deps are the loop's injectable dependencies: the work it draws from and what it
// does with each item (ADR-0019), the host its own chores act through, the knobs
// it acts by, the clock it waits on, and the sink it narrates to (ADR-0014).
type Deps struct {
	// Source is where the work comes from and where its claims are kept.
	Source Source
	// Pipeline is what happens to one item.
	Pipeline Pipeline
	// Host is every daemon chore that leaves the process.
	Host Host
	// Limits are the cadences, ceilings and thresholds.
	Limits Limits
	// Clock is the time seam; unset means [SystemClock].
	Clock Clock
	// Log is the narration sink.
	Log Narrator
}

// Run is the daemon spine. Startup clears the stale STOP sentinel, checks the
// credentials (exiting 1 if they are rejected) and fetches main; then it loops:
// stop-check → select → (idle+re-poll if empty | run+fetch if an item) — winding
// down cleanly between items on a stop request. It
// returns the process exit code: 0 on a deliberate stop or empty-then-stop, the
// daemon never failing merely because the queue ran dry.
func Run(d Deps) int {
	// Startup: clear any stale STOP left by a prior run BEFORE the first stop-check,
	// so a leftover sentinel can't abort a fresh daemon before it does any work.
	if err := d.Host.ClearStopFile(); err != nil {
		d.Log.Event("loop … warning: could not clear stale STOP sentinel: " + err.Error())
	}
	if err := d.Host.CheckAccess(); err != nil {
		d.Log.Structured(loopstream.Record{Kind: loopstream.KindLoopStopped, Message: "loop — not starting: " + err.Error()})
		return 1
	}
	if err := d.Host.FetchMain(); err != nil {
		d.Log.Event("loop … warning: could not fast-forward origin/main at startup: " + err.Error())
	}

	clock := d.clock()
	start := clock.Now()

	b := newBreaker(d.Limits.MaxConsecutiveFailures)
	attempted := 0
	for {
		// Stop only ever lands BETWEEN items — a graceful, per-item checkpoint
		// (DESIGN.md "Stop control"): stopping mid-item would strand a worktree.
		if d.Host.StopRequested() {
			return d.stopped("stop requested")
		}

		// Optional AFK wall-clock ceiling: once the daemon has run for MaxRuntime, wind
		// down on the same clean stop path between items — a deliberate stop, not a
		// failure (DESIGN.md §cmd/loop config knobs). Zero/negative = unlimited.
		if d.Limits.MaxRuntime > 0 && clock.Now().Sub(start) >= d.Limits.MaxRuntime {
			return d.stopped("max runtime reached")
		}

		// Optional AFK ceiling: once MaxTickets items have been attempted, wind down
		// on the same clean stop path as STOP — a deliberate stop, not a failure
		// (DESIGN.md §cmd/loop config knobs). Checked here, between items, so the
		// (N+1)th item is never even selected. Zero/negative = unlimited.
		if d.Limits.MaxTickets > 0 && attempted >= d.Limits.MaxTickets {
			return d.stopped("max tickets reached")
		}

		// Reclaim disk between items, before selecting the next one, when no sandbox
		// is active (ADR-0027). Gated on a cheap statfs so a healthy disk costs nothing;
		// a failed prune is narrated and swallowed — reclaim is never an item outcome
		// and never touches the breaker.
		d.reclaimDisk()

		// Reap claims stranded by a dead agent (no branch/PR past the TTL), returning
		// them to the Source BEFORE selecting — so a reaped item is selectable on this
		// very pass rather than occupying the lane forever (BEH-677). Best-effort: a
		// failed list/release is narrated and swallowed, never an item outcome.
		d.reapStaleClaims()

		item, claim, ok, err := d.Source.Next()
		if err != nil {
			// Say plainly that this is not an empty queue — the two used to log the
			// same idle line, so a dead token idled for minutes looking healthy. The
			// error rides on the structured record so `watch` shows it too. Then idle
			// and re-poll anyway: a Source hiccup at 3am must not end the run.
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindIdle, Message: fmt.Sprintf("loop — could not read the ticket queue (not an empty queue): %v; re-polling in %s", err, d.Limits.PollInterval)})
			d.idleWait()
			continue
		}
		if !ok {
			// The daemon difference from `pipeline --next`: an empty queue is a
			// normal steady state, so the loop idles for PollInterval and re-polls
			// rather than exiting (DESIGN.md "The loop"). The idle is broken into
			// short ticks that re-check stop, so a SIGINT/sentinel landing mid-idle
			// is honoured within one TickInterval, not a whole poll interval later.
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindIdle, Message: fmt.Sprintf("loop — queue empty; re-polling in %s", d.Limits.PollInterval)})
			d.idleWait()
			continue
		}
		// Run the Pipeline over the claimed item. Count it as attempted before
		// any outcome branching — a cap-aborted run was still an attempt, so it counts
		// toward the MaxTickets ceiling like any other.
		key := item.Key
		outcome := d.Pipeline.Run(item, claim)
		attempted++

		// A spending-cap abort is a control-flow signal, NOT a ticket verdict
		// (DESIGN.md §Spending-cap abort backoff): an external Anthropic cap aborted
		// the run before it could ship, so no progress was made and it is not the
		// diff's fault. Release the ticket back to Todo (don't strand it In Progress),
		// leave the breaker untouched (it stays blind to the cap runaway by design),
		// and enter a long interruptible backoff before re-polling — auto-resuming once
		// the cap window resets. A shipped PR wins, and the pipeline's fold already
		// settled that: a CapAborted run is one where nothing shipped.
		if outcome.Disposition == stages.CapAborted {
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindCapAbort, Ticket: key, Message: "loop — spending-cap abort on " + key + "; releasing to Todo"})
			if err := claim.Release(); err != nil {
				d.Log.Event("loop … warning: could not release " + key + " to Todo after cap abort: " + err.Error())
			}
			d.comment(key, "Autonomous run aborted by the Anthropic spending cap before it could ship — released back to Todo. The daemon backs off and auto-resumes once the cap window resets, so this should retry on its own.")
			d.capBackoffWait(key, outcome.CapResetAt)
			continue
		}

		// A Docker-preflight abort is environmental, NOT a ticket verdict: the host
		// couldn't launch a sandbox (full disk / daemon down) before any work began, so
		// no progress was made and it is not the diff's fault. The implementation stage
		// already released the selected ticket back to Todo through the run's lease
		// (ADR-0003), so here we only reclaim disk (the usual cause — now including the
		// Docker build-cache/image prune) and back off briefly before re-polling. The
		// breaker stays blind to it (breaker.record), so a poison top-of-queue ticket
		// can't rack up identical ~2s preflight failures and trip the breaker in seconds
		// — the pattern that was stopping the daemon and forcing a manual relaunch. Like
		// a cap abort, the disposition already settles that nothing shipped.
		if outcome.Disposition == stages.PreflightAborted {
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: key, Message: "loop — Docker preflight abort on " + key + " (environmental: full disk / daemon down); reclaiming disk and backing off before re-poll — breaker-neutral"})
			d.reclaimDisk()
			d.interruptibleSleep(d.Limits.PollInterval)
			continue
		}

		// Fetch main again — "pull main after every session" keeps the next ticket's
		// merge-base honest across the loop's long run.
		if err := d.Host.FetchMain(); err != nil {
			d.Log.Event("loop … warning: could not fast-forward origin/main after ticket: " + err.Error())
		}

		// A no-PR run is not always a failure to release: the branch may already carry a
		// committed, gate-green fix that simply never got pushed — the recovered-checkpoint
		// / verify-only state where the cold review couldn't emit its verdict, so the review
		// stage never pushed (BEH-713). Releasing it to Todo just re-grabs it to the same
		// no-verdict conclusion forever (the BEH-649 infinite bounce). So before treating a
		// no-PR run as a failure, try to FINISH it host-side — push + open the PR. A
		// successful completion is folded in as the ship it is (Shipped), so the
		// switch below skips the release and the breaker records a success (reset). Only a
		// NoPR run qualifies: a cap abort was handled + continued above, a ship has nothing
		// to finish, and a recommend-close is a genuine no-op rather than an unshipped fix.
		// A completion that still can't reach a PR, or nothing to recover, falls through to
		// the normal release below.
		if outcome.Disposition == stages.NoPR {
			if completed, attempted := d.Pipeline.Recover(item); attempted {
				if completed.Disposition == stages.Shipped {
					d.Log.Structured(loopstream.Record{
						Kind:    loopstream.KindPROpened,
						Ticket:  key,
						Message: "loop — " + key + " had a committed, gate-green fix that was never pushed; completed it (pushed + opened PR) instead of releasing (BEH-713)",
					})
					outcome.Disposition = stages.Shipped
				} else {
					d.Log.Event("loop … " + key + " committed-fix recovery did not reach a PR; falling through to the no-PR release")
				}
			}
		}

		// Recommend-close (BEH-603): the run concluded the branch makes zero net change
		// against origin/main — a duplicate/superseded ticket the review stage correctly
		// declined to ship as an empty-commit PR. Unlike every other no-PR mode this one
		// must NOT release back to Todo: releasing would let the dispatch guard re-grab it
		// and re-run the whole pipeline to the same "nothing to ship" conclusion forever.
		// So close it — move it to the terminal canceled state (BEH-682) — which takes it
		// out of BOTH the --next selection pool and the reaper pool for good. (Merely
		// keeping it In Progress, the prior disposition, was defeated by the stale-claim
		// reaper releasing it back to Todo past the TTL, re-looping forever.) Leave a
		// breadcrumb recommending exactly that (distinct from the generic "released to Todo"
		// note, which would mislead). It is breaker-neutral (handled in b.record), so it
		// falls through to the normal after-ticket fold below.
		switch outcome.Disposition {
		case stages.RecommendClose:
			closed := d.close(claim)
			msg := "loop — " + key + " makes no net change against main; closing it as a duplicate/superseded (BEH-682)"
			// The breadcrumb must reflect what actually happened: only claim the ticket was
			// moved to Canceled when the close succeeded, else a Linear/tracker hiccup would
			// post "closed" on a ticket still In Progress and suppress the human safety valve.
			note := "This ticket is a duplicate/superseded, so the run closed it (moved it to Canceled) rather than shipping an empty-commit change; reopen it if that was wrong."
			if !closed {
				msg = "loop — " + key + " makes no net change against main; could not auto-close — left In Progress for a human to close as a duplicate/superseded (BEH-603)"
				note = "This ticket is a duplicate/superseded and should be closed, but the run could not auto-close it — please close it manually as a duplicate/superseded. It was left In Progress."
			}
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindRecommendClose, Ticket: key, Message: msg})
			d.comment(key, "Autonomous run found this branch makes zero net change against `main` (an empty diff) — there is nothing to ship. "+note+" If a PR was already opened (the branch only became a no-op after the pre-push rebase) it is a no-op and should be closed too. It was deliberately NOT released to Todo so it won't be re-picked and re-run to the same conclusion. See the run logs for the cause.")
		case stages.NoPR:
			// Release-on-no-PR (BEH-590): a run that finished WITHOUT opening a pushed PR —
			// any non-cap failure mode: OOM, sandbox crash, a review stage that died before
			// pushing, an empty-diff verification failure — left the ticket claimed In
			// Progress with nothing to show for it. The dispatch claimed it on select, so
			// unless we undo the claim the board reads as "in flight" forever and the
			// dispatch guard never re-grabs it. The cap-abort branch above already released
			// (and continued), so this only fires for the non-cap no-PR case. A shipped PR
			// (a Shipped disposition) is the success signal — never released, even on a non-zero
			// exit (CI red after the auto-fix budget). Best-effort: a Linear hiccup here is
			// warned, never fatal — the daemon must keep running.
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: key, Message: "loop — " + key + " produced no PR; releasing back to Todo so it isn't stranded In Progress (BEH-590)"})
			if err := claim.Release(); err != nil {
				d.Log.Event("loop … warning: could not release " + key + " to Todo after a no-PR run: " + err.Error())
			}
			d.comment(key, "Autonomous run finished with no PR for this ticket — released back to Todo so a later run can re-grab it. Repeated occurrences here mean the ticket keeps failing to ship; see the run logs for the cause.")
		}

		// Fold the item into the breaker. The success signal is "did it reach a
		// pushed PR?" — not the pipeline exit code — so a hard-but-shipped item
		// (retrospective failed, CI red after budget) resets the counter, while an
		// item that never produced a PR increments it (DESIGN.md §Circuit breaker).
		b.record(key, outcome)
		if b.tripped() {
			// Trip on the SAME graceful wind-down path as a STOP (exit, not pause): it
			// forces a human to investigate before more items are consumed, since 3
			// identical failures are almost always environmental (expired auth, broken
			// base build) that would sink the next item too.
			d.Log.Structured(loopstream.Record{Kind: loopstream.KindBreakerTrip, Ticket: key, Message: "loop ✗ " + b.report() + " — winding down; fix the environment and relaunch"})
			return d.stopped("circuit breaker tripped")
		}
	}
}

// stopped emits the daemon's terminal record and returns the clean exit code (0).
// Unlike a plain Event (console only), a Structured KindLoopStopped record reaches
// the global loop.jsonl stream the viewer tails — so the wind-down is the definitive
// "loop stopped" marker in BOTH the logs and the animation, never confused with a
// crash or a wedged-but-alive daemon (BEH-613). reason names why it wound down.
func (d Deps) stopped(reason string) int {
	d.Log.Structured(loopstream.Record{Kind: loopstream.KindLoopStopped, Message: "loop — stopped: " + reason})
	return 0
}

// comment posts a best-effort breadcrumb on a released item (BEH-590). A post
// failure is narrated rather than fatal — the release already happened, and a
// missing breadcrumb must never sink the daemon (mirrors AddComment's best-effort
// contract).
func (d Deps) comment(key, body string) {
	if err := d.Source.Comment(key, body); err != nil {
		d.Log.Event("loop … warning: could not comment on " + key + " after release: " + err.Error())
	}
}

// close moves a recommend-close item to the terminal canceled state (BEH-682)
// and reports whether it succeeded. It is best-effort like comment: a close failure
// degrades to false, and the caller falls back to the pre-BEH-682 disposition (keep
// In Progress for a human to close) rather than crashing the daemon — a tracker
// hiccup must never sink the loop.
func (d Deps) close(claim *lease.Lease) bool {
	if err := claim.Close(); err != nil {
		d.Log.Event("loop … warning: could not close " + claim.Key() + " as superseded (leaving it In Progress for a human): " + err.Error())
		return false
	}
	return true
}

// reclaimDisk proactively frees host disk between items when free space has
// fallen below the soft DiskReclaimThreshold, BEFORE the hard MinFreeDiskBytes
// sandbox floor would refuse a launch (ADR-0027). The named goal is dead worktrees,
// so the squash-merge-aware worktree prune runs first; if free space is still below
// the threshold afterwards, the Consumer's cache prune is a cheap, non-destructive
// follow-up.
//
// Three invariants, all from ADR-0027:
//   - Gated on a cheap statfs: a healthy disk (or a zero threshold, or an
//     unreadable statfs) makes no prune / gh / shell calls at all.
//   - Never an item outcome: a failed statfs/prune is narrated and swallowed; the
//     loop continues and the circuit breaker is never touched (the breaker keys only
//     on "did the item reach a pushed PR?", ADR-0004).
//   - Narrate only when it acts: a reclaim line is emitted only when a prune actually
//     removed a worktree or moved free disk up; a healthy or no-op iteration is silent.
//
// The reclaims escalate cheapest-first, each gated on free disk STILL being below the
// threshold: merged worktrees, then the Consumer's cache prune, then the Docker
// build-cache / image prune last (usually the biggest win but the heaviest step, and the one the
// other two never touch — ADR-0027 / the DiskReclaimHint ordering).
func (d Deps) reclaimDisk() {
	if d.Limits.DiskReclaimThreshold == 0 {
		return // reclaim disabled
	}
	before, err := d.Host.FreeDisk()
	if err != nil {
		// A statfs error must never block the loop — degrade to a warning and carry on,
		// the same way the sandbox preflight treats an unreadable statfs as non-blocking.
		d.Log.Event("loop … warning: could not statfs worktrees volume for disk reclaim (skipping): " + err.Error())
		return
	}
	if before >= d.Limits.DiskReclaimThreshold {
		return // disk healthy — silent, no shell-out
	}

	removed, perr := d.Host.PruneMergedWorktrees()
	if perr != nil {
		d.Log.Event("loop … warning: pruning merged worktrees failed during disk reclaim (continuing): " + perr.Error())
	}

	// Re-statfs after the worktree prune, then escalate the cheaper-first secondary
	// reclaims only while free disk stays below the threshold. Track the latest reading
	// so the narration can report the bytes freed.
	after := before
	if mid, merr := d.Host.FreeDisk(); merr == nil {
		after = mid
	}
	// Secondary: the Consumer's cheap, non-destructive cache prune.
	if after < d.Limits.DiskReclaimThreshold {
		if serr := d.Host.CachePrune(); serr != nil {
			d.Log.Event("loop … warning: cache prune failed during disk reclaim (continuing): " + serr.Error())
		} else if final, ferr := d.Host.FreeDisk(); ferr == nil {
			after = final
		}
	}
	// Tertiary: the Docker build-cache/image prune — usually the harness's biggest
	// disk hog and the one the worktree/store prunes can't reach, but the heaviest
	// step, so it runs last and only when the cheaper prunes didn't clear the floor.
	if after < d.Limits.DiskReclaimThreshold {
		if derr := d.Host.DockerPrune(); derr != nil {
			d.Log.Event("loop … warning: docker prune failed during disk reclaim (continuing): " + derr.Error())
		} else if final, ferr := d.Host.FreeDisk(); ferr == nil {
			after = final
		}
	}

	// Narrate only when a reclaim actually acted (ADR-0027): a worktree was removed, or
	// a cache/Docker prune moved free disk up. A no-op iteration stays silent.
	if removed > 0 || after > before {
		var msg string
		if removed > 0 {
			msg = fmt.Sprintf("loop — reclaimed %d merged worktree(s) under disk pressure", removed)
		} else {
			msg = "loop — reclaimed host disk under pressure"
		}
		if after > before {
			msg += fmt.Sprintf("; freed %d MiB", (after-before)>>20)
		}
		d.Log.Event(msg)
	}
}

// reapStaleClaims releases claims stranded by a dead agent back to their Source
// (BEH-677). The harness claims an item on selection (for a ticket, by moving it to
// In Progress), but an agent that dies (sandbox OOM/timeout/crash) before pushing
// leaves the item occupying the lane forever, never re-grabbed. Each pass lists the
// Source's claims and releases a claim when ALL hold: it is older than ClaimTTL, has no linked PR,
// and has no remote branch.
//
// The grace TTL is load-bearing: a healthy agent between claim and first push looks
// IDENTICAL to a dead one (no branch/PR yet), so reaping on the no-branch signal
// alone would kill live work (the BEH-447 race). The TTL is the only thing that tells
// them apart, so it is never skipped.
//
// Best-effort, never an item outcome (like reclaimDisk): a non-positive TTL
// disables it silently; a list/release failure is narrated and swallowed so a
// Source hiccup can never crash the daemon.
func (d Deps) reapStaleClaims() {
	if d.Limits.ClaimTTL <= 0 {
		return // reaping disabled
	}
	claims, err := d.Source.Claims()
	if err != nil {
		d.Log.Event("loop … warning: could not list In Progress claims for reaping (skipping): " + err.Error())
		return
	}
	now := d.clock().Now()
	for _, c := range claims {
		if now.Sub(c.StartedAt) < d.Limits.ClaimTTL {
			continue // inside the grace window — a healthy agent may still be mid-work
		}
		if c.HasLinkedPR {
			continue // shipped or mid-review — there is work to show for the claim
		}
		if d.Source.HasRemoteBranch(c.Key) {
			continue // a branch is in flight even without a PR yet
		}
		d.Log.Structured(loopstream.Record{
			Kind:    loopstream.KindTicketReleased,
			Ticket:  c.Key,
			Message: "loop — reaping stale claim " + c.Key + " (In Progress past TTL with no branch/PR); releasing to Todo (BEH-677)",
		})
		if err := d.Source.Release(c.Key); err != nil {
			d.Log.Event("loop … warning: could not reap stale claim " + c.Key + " to Todo: " + err.Error())
			continue
		}
		d.comment(c.Key, "Autonomous reaper released this claim back to Todo: it sat In Progress past the claim TTL with no branch and no linked PR, so the agent that claimed it almost certainly died before pushing (sandbox OOM/timeout/crash). A later run can now re-grab it.")
	}
}

// idleWait waits out one PollInterval before the loop re-polls an empty queue,
// interruptibly — see interruptibleSleep. The cap-abort backoff (capBackoffWait)
// uses the same tick-and-re-check-stop mechanism with CapBackoff, additionally
// narrating its progress, so a stop is honoured within one tick during either wait
// (DESIGN.md §The loop, §Spending-cap abort backoff).
func (d Deps) idleWait() { d.interruptibleSleep(d.Limits.PollInterval) }

// clock returns the injected Clock, falling back to real time and real sleeping
// when none was wired, so the daemon and its helpers always agree on one clock.
func (d Deps) clock() Clock {
	if d.Clock == nil {
		return SystemClock{}
	}
	return d.Clock
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
func (d Deps) capBackoffWait(key string, resetTime time.Time) {
	now := d.clock().Now()
	total := d.capBackoffDuration(now, resetTime)
	wake := now.Add(total)
	wakeStr := wake.UTC().Format("15:04Z")
	d.Log.Structured(loopstream.Record{
		Kind:    loopstream.KindCapBackoff,
		Ticket:  key,
		Message: fmt.Sprintf("loop — capped; backing off %s, next re-poll ~%s", total, wakeStr),
	})

	if d.Limits.TickInterval <= 0 {
		d.clock().Sleep(total)
		d.Log.Structured(loopstream.Record{Kind: loopstream.KindCapBackoff, Ticket: key, Message: "loop — cap backoff elapsed; re-polling for " + key})
		return
	}

	var sinceBeat time.Duration
	for waited := time.Duration(0); waited < total; waited += d.Limits.TickInterval {
		if d.Host.StopRequested() {
			return // stop wins; the top-of-loop wind-down narrates the exit, not a wake record
		}
		d.clock().Sleep(d.Limits.TickInterval)
		sinceBeat += d.Limits.TickInterval
		// Heartbeat only between ticks, never on the final one (it coincides with the
		// wake record below), so the bookends never double up.
		if d.Limits.CapBackoffHeartbeat > 0 && sinceBeat >= d.Limits.CapBackoffHeartbeat && waited+d.Limits.TickInterval < total {
			remaining := total - (waited + d.Limits.TickInterval)
			d.Log.Structured(loopstream.Record{
				Kind:    loopstream.KindCapBackoff,
				Ticket:  key,
				Message: fmt.Sprintf("loop — still capped; ~%s remaining, re-poll ~%s", remaining, wakeStr),
			})
			sinceBeat = 0
		}
	}
	d.Log.Structured(loopstream.Record{Kind: loopstream.KindCapBackoff, Ticket: key, Message: "loop — cap backoff elapsed; re-polling for " + key})
}

// capResetMargin is added past the parsed reset time so the daemon wakes just AFTER the
// cap clears rather than a hair before it (which would instantly re-abort). Small: the
// cap resets at the named minute, so a couple of minutes absorbs clock skew between the
// account's timezone and the host without wasting the window.
const capResetMargin = 2 * time.Minute

// maxCapResetBackoff is the sanity ceiling on a reset-derived wait (BEH-708). The abort
// message carries a bare clock time with no date or timezone, so a skewed or
// wrapped-to-tomorrow resolution can land far in the future; cap windows reset within a
// few hours, so anything beyond this almost certainly means the parse mis-resolved and
// the fixed CapBackoff is the safer wait.
const maxCapResetBackoff = 6 * time.Hour

// capBackoffDuration is how long to sleep after a spending-cap abort. When the abort
// message named an exact reset time (BEH-708, threaded in as resetTime), it waits until
// that reset plus capResetMargin so the daemon resumes right when the cap clears —
// replacing both the fixed-duration guess and the ~5-minute re-poll churn with a single
// wait. It falls back to the fixed CapBackoff when no reset time was parsed (zero value),
// when the reset already passed (non-positive wait), or when the wait is implausibly long
// (> maxCapResetBackoff — a mis-resolved parse).
func (d Deps) capBackoffDuration(now, resetTime time.Time) time.Duration {
	if resetTime.IsZero() {
		return d.Limits.CapBackoff
	}
	wait := resetTime.Sub(now) + capResetMargin
	if wait <= 0 || wait > maxCapResetBackoff {
		return d.Limits.CapBackoff
	}
	return wait
}

// interruptibleSleep waits out total, but breaks the wait into TickInterval chunks
// and re-checks StopRequested between each chunk. A stop (SIGINT/sentinel) landing
// mid-wait is therefore observed within one TickInterval, not a whole total later —
// the responsiveness the daemon's "idle/backoff is an interruptible stop point"
// (ADR-0004) depends on. A non-positive TickInterval degrades to a single total
// sleep so a misconfigured tick can't spin.
func (d Deps) interruptibleSleep(total time.Duration) {
	if d.Limits.TickInterval <= 0 {
		d.clock().Sleep(total)
		return
	}
	for waited := time.Duration(0); waited < total; waited += d.Limits.TickInterval {
		if d.Host.StopRequested() {
			return
		}
		d.clock().Sleep(d.Limits.TickInterval)
	}
}
