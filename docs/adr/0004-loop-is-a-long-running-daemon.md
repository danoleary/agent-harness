# ADR-0004: The autonomous loop is a long-running daemon, not drain-and-exit + cron

- Status: Accepted
- Date: 2026-06-28

## Context

The three single-role tools (`implementation`, `review`, `retrospective`) and the
single-ticket `pipeline` (hand-passed or `pipeline --next`) all run **once and
exit**. The remaining piece is the autonomous loop: "select a ticket, run the
pipeline, repeat" over the `ready-for-agent` queue (DESIGN.md "The loop").

There are two shapes for that loop, and they are different machines:

- **Drain-and-exit + external timer.** A process that works every eligible ticket
  back-to-back and **exits the moment the queue is empty**; an external scheduler
  (cron/launchd timer) re-launches it every N minutes to pick up newly-labelled
  work. This is what DESIGN.md originally specified (`if no ticket -> exit
  cleanly`).
- **Long-running daemon.** A process that **stays alive across an empty queue** —
  on no eligible ticket it idles and re-polls, so a ticket labelled
  `ready-for-agent` an hour later is picked up by the *same* running process with
  no re-launch.

The choice is load-bearing: it dictates whether "queue empty" is terminal or
merely idle, whether stop control must be responsive *during* a sleep, and how the
process is supervised. It is also genuinely surprising in context — a process that
holds `LINEAR_API_KEY` and `GH_TOKEN` live for days is exactly what a future reader
or a security reviewer will question, the more so because scheduled/cron-style
runners are the usual reach for unattended work.

## Decision

**The loop is a long-running daemon** (`cmd/loop`, a standalone binary over the
shared `internal/`). On an empty queue it **idles and re-polls** rather than
exiting; it runs until a deliberate stop, a circuit-breaker trip, or an optional
`LOOP_MAX_*` ceiling. It is started as a **standalone detached process** the
operator walks away from (`nohup bin/loop &`) — no tmux, no launchd/systemd
supervisor in v1.

The daemon shape pulls in the apparatus that makes it safe to leave running:

- **Idle is an interruptible stop point.** The empty-queue sleep is broken into
  short ticks that re-check `stopRequested || exists(STOP)`, so a `touch
  agent-harness/STOP` is honoured within seconds rather than after a full poll
  interval. The STOP sentinel is cleared at clean startup so a stale file can't
  instantly kill a fresh launch.
- **A spending-cap abort is a control-flow signal, not a verdict.** An external
  Anthropic cap (`session.Outcome.SpendingCapAbort`) aborts *every* session at
  start; left as-is the daemon would spin the whole queue producing nothing. So a
  cap-abort releases the ticket to Todo, leaves the circuit breaker untouched
  (it is not a failure), and enters a long interruptible backoff before
  auto-resuming.
- **Exit-code contract.** `0` = a deliberate terminal stop (STOP, breaker, or a
  `LOOP_MAX_*` ceiling); non-zero = an unexpected crash. With no supervisor
  nothing acts on it, but it keeps "stopped on purpose" distinguishable from
  "died" and lets a future launchd `KeepAlive.SuccessfulExit=false` not resurrect
  a breaker trip.

## Alternatives considered

- **Drain-and-exit + cron/launchd timer (the originally-documented shape).**
  Smaller credential blast radius (the process is alive only while there is work),
  a simpler single-shot process, and it reuses `pipeline --next`'s existing
  "exit 0 on empty queue" verbatim. Rejected because the operator explicitly wants
  *continuity* — a ticket labelled mid-afternoon should be picked up without anyone
  re-launching — and because a cron cadence adds its own dependency and a re-launch
  latency (a ticket waits up to the cron interval before any work starts). The
  daemon buys continuous pickup at the cost of the stop/idle/backoff apparatus
  above, which we judged worth building once.
- **Long-running daemon under launchd/systemd from day one.** Boot survival and
  crash auto-restart, OS-managed. Rejected for v1 as premature: a supervisor that
  blindly restarts would resurrect a STOP or a breaker trip (defeating the "force a
  human to look" purpose) unless carefully gated on the exit code, and that gating
  is only worth wiring once a bare standalone process has proven out. The
  exit-code contract is deliberately defined now so launchd can be added later
  without rework.

## Consequences

- `cmd/loop` carries daemon-only concerns the single-shot `pipeline` never has to
  reason about: idle re-poll, interruptible sleep, spending-cap backoff, the
  circuit breaker, and the STOP-file lifecycle. The tested single-shot path is
  untouched.
- The process holds real credentials (`LINEAR_API_KEY`, `GH_TOKEN`) live for as
  long as it runs. The sandbox boundary is unchanged (agents still get only the
  Claude credential, ADR-0002); the new exposure is host-side and accepted as the
  price of continuity.
- Stop control gains an idle path and a startup-clear; the circuit breaker keys on
  "did the ticket reach a pushed PR?" (not the pipeline exit code) so meta-step
  failures don't trip it. Both are detailed in DESIGN.md.
- No supervisor in v1 means a crash or a breaker trip stays down until the operator
  relaunches — intended for the breaker, acceptable for a crash. launchd/systemd is
  a documented future step, unblocked by the exit-code contract.
- A "poison" top-of-queue ticket (released to Todo on a no-worktree crash, then
  re-selected) trips the breaker within 3 iterations rather than spinning forever;
  quarantine-and-continue is a deferred follow-up, not built in v1.
