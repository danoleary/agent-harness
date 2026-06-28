# ADR-0005: The loop viewer is a separate read-only reader over a structured event stream

- Status: Accepted
- Date: 2026-06-28

## Context

The autonomous loop is a detached daemon (`nohup bin/loop &`, ADR-0004) that an
operator walks away from. Its only window today is `loop.log` — the raw stdout
stream, which interleaves the concise harness narration (`⚒ Bash`, `launching
sandbox (cap 30 min)`) with Docker build output and, under `--verbose`, the whole
agent transcript. To answer "what ticket is it on, which stage, is it alive?" an
operator greps a noisy file or globs `logs/BEH-*/run.jsonl` and guesses the active
ticket from filesystem mtimes.

We want a friendlier live view: the current ticket, which of the three stages is
running, the current step, daemon health, and an at-a-glance animated indicator.
Three facts about the loop constrain how that view can be built:

1. **The daemon runs detached** — it has no attached terminal in the normal run
   model (DESIGN.md "Run model"). A `--tui` flag that draws to the daemon's own
   stdout would have no screen to draw to.
2. **The harness is deliberately zero-dependency** — standard library only,
   "because this process holds real credentials" (DESIGN.md "Harness runtime").
   A rich TUI framework (bubbletea/lipgloss) is a transitive dep tree.
3. **There is no progress percentage.** Work is three discrete stages
   (implementation → review → retrospective); within a stage the only live signal
   is a stream of tool-use events. Nothing knows "how far along" a stage is.

## Decision

**The viewer is a separate, read-only binary (`cmd/watch`) that tails a global
structured event stream the daemon writes.** It does not control the loop — `touch
agent-harness/STOP` remains the only control path — and quitting it never touches
the daemon.

- **A global structured stream, `logs/loop.jsonl`.** The shared narration sink,
  which every entrypoint already routes through, additionally appends a structured
  record per event: `{ts, kind, ticket, stage, message, detail}`. `message` is the
  existing human string verbatim (so the stream is a strict superset of today's
  console line); `kind` is a closed enum (`ticket-selected`, `stage-start`,
  `sandbox-launch`, `tool-use`, `session-result`, `pr-opened`, `ticket-released`,
  `cap-abort`, `breaker-trip`, `idle`) that the viewer switches on without
  re-parsing prose. The daemon truncates the file at clean startup, like `loop.log`,
  so it is bounded to one daemon run. Because the record is emitted at the shared
  sink, single-shot `pipeline`/stage runs produce the same stream and are viewable
  too.
- **The viewer is stdlib-only.** It is a redraw-on-a-ticker dashboard — current
  ticket panel, stage indicator (n of 3), a live current-step line, a short
  scrollback tail (the last few events), and a stateful ASCII mascot (labelled
  `status:`) whose mood (working / waiting / sleeping / celebrating / hurt /
  stopping) is driven by the last `kind`. This needs no component model, mouse, or
  raw-mode input (Ctrl-C quits), so it is hand-rolled ANSI with no new entry in the
  harness `go.sum`.
- **It surfaces the loop's control + liveness state, read-only.** Beyond the event
  stream the viewer probes three environmental facts each tick — the daemon pidfile
  (alive/down), the `STOP` sentinel (a present sentinel ⇒ "winding down", which
  overrides the event-derived mood and is the health headline), and whether the
  stream file exists at all (an absent stream on a live daemon ⇒ "no event stream",
  distinct from a present-but-empty "no events yet"). All three are pure reads
  (`os.Stat`/signal-0): the viewer displays the operator's stop request but never
  writes the sentinel — `touch agent-harness/STOP` remains the only control path.
- **It degrades to plain text.** When stdout is not a TTY, or `NO_COLOR` is set,
  the viewer prints plain scrollback lines (and no mascot) instead of the animated
  screen, so piping/redirecting it stays clean. `--no-animation` is a separate,
  reduced-motion toggle: it keeps the full dashboard but freezes the mascot to a
  single static frame (the rest of the view still updates live), rather than dropping
  to plain text.

## Alternatives considered

- **A `--tui` flag on `bin/loop` itself.** No second binary, no second stream.
  Rejected because the daemon runs detached with no TTY (fact 1); the flag would
  only ever help the rare foreground-babysitting case, and it forces the
  dependency/TTY questions onto the credential-holding daemon.
- **Tail and parse the existing `loop.log`.** Zero daemon changes. Rejected: the
  stream is noisy (Docker build output, `--verbose` transcripts) and unstructured,
  so the viewer would reconstruct meaning by matching on emoji prefixes and prose
  that has churned repeatedly across tickets — a brittle contract.
- **Watch the per-ticket `logs/BEH-*/run.jsonl` files.** Cleaner per-ticket data,
  but no global ordering and the active ticket must be inferred from mtimes. A
  single global stream removes the directory-scan race.
- **A TUI framework (bubbletea/lipgloss).** Richer interactivity. Rejected for v1
  because the view is read-only and simple enough for hand-rolled ANSI, and adding
  it to the shared module pollutes the harness `go.sum` even though `bin/loop`
  never links it. If a *rich interactive* viewer is ever wanted, the documented
  next step is a **separate Go module** for the viewer so its deps never reach the
  harness module — not a dep added to the harness module.

## Consequences

- The `loop.jsonl` schema becomes a load-bearing contract between daemon and
  viewer: the `kind` enum and field set cannot change without updating both. This
  is the deliberate cost of not parsing prose.
- The viewer holds **no** credentials — it only reads log files — so the
  zero-dependency justification (credential blast radius) does not bind it; staying
  stdlib-only is a posture choice (one module, clean `go.sum`), not a security
  requirement, and the escape hatch (separate module) is recorded above.
- The daemon change is additive and tiny: one extra structured append at the
  existing sink. The console line and per-ticket `run.jsonl` are unchanged, so
  every existing reader and the tested single-shot path are untouched.
- "Progress" in the viewer is honest about being indeterminate: a stage indicator
  (n of 3) and a tool-call activity counter, never a percent-complete bar.
