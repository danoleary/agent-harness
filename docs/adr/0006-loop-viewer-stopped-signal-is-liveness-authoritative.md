# ADR-0006: The loop viewer's "stopped" signal is liveness-authoritative and loud

- Status: Accepted
- Date: 2026-06-30

## Context

ADR-0005 made the loop viewer (`cmd/watch`) a read-only dashboard whose ASCII
mascot mood is **driven by the last event's `kind`** in `loop.jsonl`. BEH-613 then
added a definitive `KindLoopStopped` terminal record plus a `pigStopped` mascot, so
a *clean* loop exit (STOP requested, max-runtime, max-tickets, breaker trip) shows a
"stopped" mascot.

Operators still report that **the viewer does not clearly display that the agent has
stopped.** Reconstructing why exposed a structural gap: the two halves of the same
screen derive "is it stopped" from different sources and can flatly contradict each
other.

- The **health line** is liveness-driven — `renderHealth` reads the pidfile
  signal-0 probe (`status.Alive`) and correctly says `○ daemon down` whenever the
  process is gone, however it died.
- The **mascot** is event-driven — `mood = d.pig`, set only from the event stream.
  It reaches `pigStopped` *only* via a `KindLoopStopped` record.

A crash, OOM `kill -9`, `pkill`, or host sleep never emits `KindLoopStopped`. So the
process is gone, the health line quietly reads "daemon down", and the **dominant
visual — the mascot — keeps showing the last (usually "working") mood, actively
claiming the agent is fine.** Even the clean path is weak: `pigStopped` is nearly
identical to the sleeping pig, and the 500ms redraw ticker keeps animating a dead
screen, which reads as "alive".

There are three distinct ways the loop stops being useful, and only the first was
legible:

1. **Clean exit** — terminal `KindLoopStopped` emitted; process then gone.
2. **Unclean death** — process gone, no terminal event (crash / OOM / kill).
3. **Wedged-but-alive** — process up, but no events for a long time.

The operator's stated requirement is to be told **whenever any of the three
happens.** ADR-0005's constraints carry over: read-only (never controls the loop),
stdlib-only hand-rolled ANSI, and graceful degradation to plain text under
`NO_COLOR` / non-TTY.

## Terminology

Resolved during design, because the harness overloaded these words:

- **Stopped** — the process is not alive (`status.Alive == false`), *for any
  reason*. This collapses the former separate words "stopped" (clean exit) and
  "down" (process gone) into one concept, because under the decision below they are
  the same state.
- **Stopping** — a `STOP` sentinel is present but the process is still alive
  (winding down).
- **Stalled** — the process is alive but has emitted no event past a staleness
  threshold (the new name for scenario 3).

## Decision

**Process liveness — not the event stream — is the authority for the "stopped"
signal, and that signal is rendered loudly across more than one channel.**

1. **Liveness is the authority for the stopped mascot.** `Render` computes mood as:
   `!status.Alive` → `pigStopped`, winning over the event-derived `d.pig` *and* over
   the `StopRequested` "stopping" wave. The pidfile probe already covers **both**
   clean exit and unclean death (both end with the process gone), so this single
   rule fixes scenarios 1 and 2 together. `KindLoopStopped` is **demoted** from "the
   trigger for the stopped mascot" to "the carrier of the stop *reason*". This
   deliberately refines ADR-0005's "mood driven by the last `kind`" and BEH-613.

2. **The stopped signal is loud and multi-channel** — no single channel survives
   every context, so they reinforce:
   - **Freeze the animation when not `Alive` (or Stalled).** Motion is reserved for
     "alive and working"; a frozen frame is itself a signal. This holds regardless
     of the `--no-animation` flag (which already freezes the live case).
   - **A full-width banner headline above the mascot**, carrying the state and
     reason — impossible to scan past.
   - **Color as reinforcement, never the sole signal** — red for Stopped, amber for
     Stalled. Safe here because `NO_COLOR` already drops to the mascot-less plain
     fallback, and it keeps faith with the product's "never by colour alone"
     principle even though this is a dev tool.
   - **A terminal bell (`\a`) emitted once on the *transition* into Stopped or
     Stalled** (state-change edge, not every tick), so a backgrounded `watch` still
     pokes an operator who walked away. Suppressed under `NO_COLOR` or `--no-bell`.

3. **The banner distinguishes clean from unclean exit**, because the operator's next
   action differs:
   - `Alive == false` **and** a `KindLoopStopped` record was seen → **clean**; show
     its reason, e.g. `⏹  AGENT STOPPED — max-tickets reached`.
   - `Alive == false` **and** no `KindLoopStopped` was ever seen → **unclean**; the
     process died without winding down. Distinct `⚠` marker and a sharper red:
     `⚠  AGENT STOPPED — exited without clean shutdown`. This is the case that most
     needs flagging (investigate, not "it finished"). There is no per-reason mascot
     art — `pigStopped` is the only art; the *words* carry the reason.

4. **Stalled detection is viewer-only and threshold-based.** When `Alive` but the
   last observed event is older than a **configurable threshold** (default generous,
   ~10 min, comfortably above a slow sandbox spin-up), escalate to an **amber
   `STALLED — no activity for Nm`** banner, worded as *suspicion* (the process is
   alive and may resume) and never using the word "stopped". This reuses data the
   dashboard already has (`last event N ago`) and needs no daemon change.

5. **The plain / non-TTY fallback gets the stopped signal in text.** It already
   prints a transition notice when the stream goes absent and renders the clean
   `KindLoopStopped` record verbatim as its own line; it is extended to probe the
   pidfile and print a one-line stopped notice on the alive→dead transition, so an
   **unclean death** (which writes no line at all) still surfaces in a pipe. The
   richer clean-vs-unclean wording and the Stalled suspicion stay dashboard-only: the
   plain tailer carries no model state (last-event age, terminal-record sighting), and
   the plain path is the redirect/pipe surface, not the operator's live window.

## Alternatives considered

- **Keep the mascot event-driven; just make `pigStopped` art louder.** Rejected: it
  cannot fix unclean death — there is no event to drive the mascot — which is the
  core complaint. The authority, not the art, is the problem.
- **Add a daemon-side `KindHeartbeat` + watchdog goroutine now (Option B).** This
  would detect a wedge precisely: the loop is single-threaded and blocks inside the
  agent session during a stage, so only a separate goroutine can heartbeat through a
  hung session. Rejected for this iteration: materially more code, and it touches the
  credential-holding daemon's threading, for a scenario the threshold already makes
  *visible*. Recorded as the upgrade path if a false "looks fine" ever bites.
- **OS desktop / push notifications for away-from-keyboard alerting.** Rejected:
  platform-specific, drags in dependencies or shells out, against the viewer's
  zero-dependency grain. The harness has a separate notification path that is the
  right home if true push is ever wanted; the read-only viewer is not.
- **Color + banner without freezing the animation.** Rejected: the perpetual 500ms
  animation is itself a "looks alive" lie, so freezing is load-bearing, not cosmetic.
- **Dim/strike the ticket/stage/step rows when stopped.** Considered to stop the
  stale data panel from looking live; rejected as complexity for little gain once the
  banner is the headline.
- **Flatten all stop reasons to a single "STOPPED" with detail left in the
  scrollback.** Rejected: clean-vs-unclean is the one distinction that changes what
  the operator does next, so it earns an in-banner marker.

## Consequences

- The viewer now treats the **pidfile as the primary stopped authority** and the
  `loop.jsonl` stream as the *reason* source rather than the stopped trigger. A
  PID-reuse race (another process grabbing the dead loop's PID) could make a dead
  loop read as alive — low probability, pre-existing in the signal-0 probe, noted
  not fixed here.
- **`KindLoopStopped` (BEH-613) remains earned** even though it no longer drives the
  mascot: it is the clean-vs-unclean discriminator and the reason carrier. Were it
  removed, every stop would render as unclean.
- New surface area: one config knob (the stall threshold) and one flag (`--no-bell`),
  both documented with their defaults.
- **Stalled is deliberately coarse and best-effort** — a threshold, surfaced as a
  suspicion. A genuine wedge that begins inside a legitimately long quiet gap will
  not flag until the threshold elapses; the precise detector is the deferred
  Option B.
- The contract widening is small and additive on the daemon side (none required for
  scenarios 1–3 beyond what BEH-613 already emits); the change is concentrated in the
  viewer's `Render` and the `cmd/watch` transition/bell logic.
