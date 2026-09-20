# ADR-0014: The daemon takes a Host, Limits and a Clock

- Status: Accepted
- Date: 2026-09-20

## Context

`internal/loop` is the daemon spine: 704 lines whose *algorithm* — startup,
select → run → repeat, idle-and-re-poll, stop between tickets, the circuit breaker
— was well factored and exhaustively tested (89.8%). Its **interface** was not.

`loop.Deps` had **27 fields**: sixteen function-typed thunks and eleven scalars.

```
ClearStopFile  FetchMain  StopRequested  ResolveNext  ReleaseTicket  CloseTicket
CommentTicket  RunPipeline  RecoverCommittedFix  ListInProgressClaims
TicketHasRemoteBranch  Sleep  FreeDisk  PruneMergedWorktrees  CachePrune  DockerPrune
ClaimTTL  PollInterval  TickInterval  CapBackoffHeartbeat  CapBackoff
DiskReclaimThreshold  MaxConsecutiveFailures  MaxTickets  MaxRuntime  Now  Log
```

Roughly 230 of the package's 704 lines were `Deps` field documentation: **the
struct describing the dependencies was longer than the logic using them.** Sixteen
thunks are sixteen seams with exactly one real adapter apiece — and one adapter is
a hypothetical seam, not a real one.

Worse, **six of the thunks were nil-checked at runtime**, so three *features* —
stale-claim reaping, committed-fix recovery, disk reclaim — were switched on by the
nil-ness of an injected func. Whether a given daemon reaped stale claims was
answerable only by reading `cmd/loop/main.go`.

The cost landed there. `cmd/loop` was **510 lines at 11% coverage**, one test, over
a 14-line string scraper. Among the untested: `dockerPrune`, `cachePrune`,
`pruneMergedWorktrees`, `killHarnessContainers`, `listStaleClaims`, `runPipeline`,
the STOP-sentinel filesystem ops — and `recoverCommittedFix`, **60 lines of
ship-critical policy**: stat worktree → `WorktreeClean` → `FetchMain` →
`BranchDiffEmpty` → `pr.OpenExists` → `RebaseOntoMain`/`AbortRebase` → re-check
`BranchDiffEmpty` → `PushForceWithLease` → `FetchTicket` → `CommitSubjects` →
`openPR`. Nine git/gh calls and six early returns deciding whether a ticket ships,
in `package main`, where no test can import it. `loop_test.go` tested that the loop
*calls a stub*, never what the real one does. Its `openPR` was a near-verbatim copy
of the review stage's `gh pr create` — two copies of "how the harness opens a PR",
in two packages, one untested.

## Decision

**The daemon takes the same shape the Stages took in [ADR-0013]: one host port,
the knobs beside it, and one clock.** `loop.Deps` goes 27 fields → 4.

```go
type Deps struct {
	Host   Host    // everything that leaves the process
	Limits Limits  // the cadences, ceilings and thresholds
	Clock  Clock   // Now + Sleep, one seam
	Log    Narrator
}
```

- **`loop.Host`** is the daemon's half of the host port — same shape as
  `hostio.Host`, wider lifetime: a Stage's host is bound to one run of one ticket,
  the daemon's outlives every ticket it works. Two adapters, which is what makes
  it a real seam: `loophost.Real` (the STOP sentinel, the tracker queue, the
  pipeline run, the disk reclaim) and the tests' scripted `fakeHost`.
- **`loop.Limits`** holds the eleven scalars. Every "is this feature on?" question
  is now answered by a number an operator sets — a zero `DiskReclaimThreshold`
  disables reclaim, a non-positive `ClaimTTL` disables the reaper — instead of by
  which funcs the composition root happened to wire. A `Host` method always exists.
- **`loop.Clock`** is `Now` + `Sleep` as one interface. They were two thunks that a
  test could fake independently, which is a daemon whose clock and sleep disagree.

**`internal/loophost` is the production `loop.Host`**, and `cmd/loop` shrinks from
510 lines to a composition root: config, stream, host, signal handler, `loop.Run`.

**`internal/ship` is the one host-side "finish a branch"** — force-with-lease push
plus templated `gh pr create` — with the BEH-713 committed-fix recovery on top of
it. The review stage and the daemon's recovery are its two callers, so the harness
has one implementation of how it ships a branch, expressed over `hostio.Host` and
therefore testable against `hostio.Fake`.

## Alternatives considered

- **Keep the thunks, drop only the nil checks.** Cheapest, and it would have fixed
  the invisible features — but it leaves sixteen one-adapter seams and a 27-field
  struct, and leaves the recovery policy in `package main`. Rejected: the nil
  checks were a symptom.
- **Reuse `hostio.Host` directly for the daemon.** Tempting — the two overlap on
  git and the tracker. But `hostio.Real` is bound to one ticket's run id and
  logger, while the daemon outlives every ticket; and the daemon's host must run
  the *pipeline*, which would make `internal/hostio` import `internal/stages`,
  which imports `internal/hostio`. Rejected: two ports, one shape.
- **Put `loophost` inside `internal/loop`.** No import cycle, fewer packages — but
  the loop's unit tests would then live in a package that imports Docker, git and
  the tracker adapters, and the port would sit in the same file as its adapter.
  Rejected: this repo keeps `hostio` (port) separate from what it drives.
- **Leave `recoverCommittedFix` in `cmd/loop` and test it through the daemon.**
  That is what the old tests did — they asserted the loop calls a stub. The stub
  is not the policy. Rejected.
- **Share the whole review-stage tail (rebase, empty-diff re-check, push, PR).**
  The two callers genuinely diverge before the push: the review stage runs a
  sandboxed conflict-resolution session and a recommend-close verdict; the recovery
  aborts and defers. Only the push+PR pair is the same operation, so only it moved.

## Consequences

- `loop.Deps` 27 fields → 4; sixteen hypothetical seams → one real one.
- **The ship-critical recovery policy is testable**: `internal/ship` is at 100%,
  covering the six early returns, the rebase conflict, the branch that empties under
  the rebase, the tracker hiccup, the failed push and the failed `gh pr create` —
  none of which any test could reach before.
- Two copies of "open a PR" → one. `internal/pr`'s templated title/body now has a
  single caller path.
- `cmd/loop` 510 lines → ~155, and the helpers it held are ordinary internal code:
  `internal/loophost` is at 58% (from 11%), with the STOP-sentinel resolution, the
  claim projection, the selection-failure fold and the cache-prune rung all covered.
- One behaviour change, deliberate: the committed-fix recovery now fetches the
  ticket for the PR body **before** pushing. A tracker hiccup used to leave the
  branch pushed with no PR — the very state the recovery exists to clean up.
- `Host.OpenPRExists` joins `hostio.Remote` (the recovery's "is this branch really
  stranded?" read), so `internal/ship` needs no `internal/pr` lookup of its own.
- The daemon's optional chores are declared where an operator can see them:
  `LOOP_DISK_RECLAIM_THRESHOLD_BYTES=0` is the reclaim off switch, and a
  non-positive `Limits.ClaimTTL` the reaper's (the env knob, `LOOP_CLAIM_TTL_MS`,
  must be positive when set, so a production daemon always reaps — which the old
  nil-lister check made impossible to tell from the code).

[ADR-0013]: 0013-stages-talk-to-the-host-through-one-port.md
