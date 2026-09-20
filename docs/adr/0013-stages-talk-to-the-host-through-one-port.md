# ADR-0013: Stages talk to the host through one port

- Status: Accepted
- Date: 2026-09-20

## Context

`internal/verify` is nine pure decision functions with a hand-written truth table,
and it sits at 100% coverage. `internal/pipeline` injects five homogeneous thunks
and sits at 96.6%. `internal/stages` — 2,762 lines holding the harness's entire
policy — sat at **16.3%**, and per function it was worse:

| function | coverage |
|---|---|
| `Implementation` (337 lines) | 0.0% |
| `Review` (541 lines) | 0.0% |
| `Retrospective` | 11.5% — reached only a 7-line precondition early-return |
| `tddCap`, `runGates`, `fixSessionError`, `retryableEnvCrash`, … | 100% |

That is not an accident of effort. Functions that were *made pure* so they could be
tested are tested; the functions that **assemble and act on them** were never given
a seam, so they could not be. All three Stage entry points shared one clean
signature — `(cfg config.Config, log *runlog.Logger, runID string, args Args)` — in
which not one of the four parameters could be substituted. The package imported 16
of the 25 internal packages and reached straight for `gitpkg.*`, `sandbox.*`,
`session.Run`, `trackers.New`, `pr.*` and `ci.*` at package level. The only way into
a Stage body was a live Docker daemon and a live tracker.

So tested predicates sat inside untested composition:

- `verify.ReviewVerdictRetry` is pure and exhaustively tested — but the fixed-point
  loop that calls it mutates five variables inside a `for` whose *condition*
  re-evaluates the predicate against the just-mutated state. The predicate was
  tested; the loop was not.
- `runGates` had three tests — but `runHostGate`, the closure supplying its
  `runOne`, held the docs-only short-circuit, per-gate container naming, per-gate
  OOM retry with backoff, and transcript teeing. Unreachable.
- `retryableEnvCrash` was tested exhaustively — but the eight-branch tree deciding
  whether to even ask it was not.

Underneath that, one shape repeated eight times. `session.Run` was called at eight
sites, each with a near-identical seven-field `session.Options` literal and its own
container-naming scheme; `ContainerPrefix(...)` was re-`Sprintf`'d at nine sites
with ad-hoc suffixes (`-retry%d`, `-launch%d`, `-postrebase`). And the invariant
that makes the session watchdog work —

> `Options.ContainerName` must equal the `--name` in the argv, or the timeout
> `docker kill` misses its target

— was restated in **four separate comments**, because nothing enforced it.

## Decision

**`internal/hostio` names the harness's host-side I/O as one port, and the three
Stage functions accept it.**

```go
func Implementation(h hostio.Host, cfg config.Config, log *runlog.Logger, args Args) Result
func Review(h hostio.Host, cfg config.Config, log *runlog.Logger, args Args) Result
func Retrospective(h hostio.Host, cfg config.Config, log *runlog.Logger, args Args) Result
```

`Host` composes four interfaces — `Sandbox` (preflight, the two container shapes,
the `--dry-run` previews), `Repo` (the checkout and one ticket's worktree),
`Remote` (fetch, the two push shapes, `gh`) and `Findings` (the tracker port plus
the three dropbox operations that reach it).

Two adapters ship, which is what makes it a real seam and not a hypothetical one:

- **`hostio.Real`** — docker, git, gh, and the Consumer-declared tracker.
- **`hostio.Fake`** — scripted outcomes and recorded calls. `NewFake()` returns a
  *healthy* host, so a test overrides only the one signal it is about.

`Fake` is deliberately in the non-test build: `internal/stages`, `internal/pipeline`
and (next) `internal/loop` all need it, and a `_test.go` fake is importable by none
of them.

Behind `Sandbox` sits a **`Runner`** owning `{cfg, log, runID, prefix, pid}`. A
caller hands it a *label* — `"implementation"`, `"gate-check"`, `"cifix-1"` — and
the Runner mints the container name, builds the argv that carries it, builds the
`session.Options` the watchdog kills through, derives the transcript filename from
the same label, and owns the transient-failure retry (a fresh name and transcript
per attempt, because a wedged container's `--rm` teardown may have failed). The
name-matches-argv invariant is now structural: a caller cannot name a container at
all.

Every `Repo`/`Remote` method is keyed by the ticket slug, so the
`(herdPath, branchPrefix, slug)` triple that `internal/git` threads through 19 call
sites in the review stage alone is bound once, in the adapter.

## Alternatives considered

- **Keep the free-function call sites and inject a `commandRunner` into
  `internal/git`.** That is the pattern `internal/git` already uses — twelve
  exported/unexported twin pairs — and it is why `git_test.go` is 1,817 lines
  asserting *which argv is emitted* rather than what happens to a repository. It
  also does nothing for the Docker, gh and tracker halves. Rejected; the twin pairs
  are their own problem, to be collapsed onto this port.
- **A `Host` per stage (three narrower interfaces).** Narrower is usually better,
  but the three overlap almost completely — all three resolve the tracker, run a
  sandboxed session, and read worktree ground truth — and one fake is cheaper to
  keep honest than three. Rejected.
- **Export the stage bodies as unexported twins taking a `Host`, keeping the old
  exported signature.** Zero call-site churn, and exactly the twin-pair shape this
  repo is already carrying too much of: the interface the world uses would be the
  one no test exercises. Rejected — the entrypoints are the composition root and
  should look like it.
- **Leave `ci.WatchAndFix` wired at the stage.** Its driver takes nine positional
  parameters, five of which are git/gh closures the host already owns. Leaving it
  there would also mean a fake-host review test still shelling out to `gh`.
  Rejected: `Host.WatchCI` takes the one thing the stage genuinely supplies — the
  sandboxed fix session.

## Consequences

- `internal/stages` goes **16.3% → 81%** covered, with `Implementation`, `Review`
  and `Retrospective` at 81/81/83% — the largest coverage gap in the repo, closed
  without changing what any of them does.
- Eight `session.Run` sites become two Runner methods; nine ad-hoc container-name
  `Sprintf`s become one. `sandbox.BuildGateRunArgs` and
  `sandbox.BuildPostCreateRunArgs`, whose bodies were byte-identical, become one
  `BuildWorktreeCommandArgs`.
- `internal/stages` stops importing `internal/git`, `internal/github`,
  `internal/semdedup`, `internal/trackers` and `internal/proc` — the ADR-0011
  upstream sink is no longer a concrete GitHub client constructed at the stage
  layer.
- **One step-log naming rule.** The host-side gate re-run used to write
  `gate-<run-id>-<gate>.log`, a shape of its own; it is now
  `gate-<gate>-<run-id>.log`, like every other step log. Container names,
  transcript names and the `--dry-run` previews are otherwise byte-identical to
  before. `runlog.GateTranscriptName` is gone.
- The `cmd/` entrypoints and `cmd/pipeline`/`cmd/loop` are the composition root:
  they build one `hostio.Real` per slice, which all three stages share (one run id,
  one tracker client).
- This is the port the next two refactors land on. `loop.Deps`' 27 fields collapsed
  onto the same shape in [ADR-0014](0014-the-daemon-takes-a-host-limits-and-a-clock.md) — a daemon-lifetime `loop.Host` beside this
  ticket-lifetime one — and the fake it ships is what made the daemon's ship-critical
  committed-fix recovery (`internal/ship`) testable. It is also what makes moving
  Ground-truth gathering behind the verify decision affordable.
