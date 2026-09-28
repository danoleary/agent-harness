# ADR-0017: ship lands the branch

- Status: Accepted
- Date: 2026-09-28
- Extends: ADR-0014, ADR-0016

## Context

ADR-0014 made `internal/ship` the one host-side "finish a branch": push
force-with-lease, then `gh pr create`. That was only the last ten lines of the
sequence. "Land this branch on the latest base" existed three times, and the copies
had drifted:

| Step | Review tail | `ship.Recover` | CI watch (`rebaseOntoBase`) |
|---|---|---|---|
| fetch + rebase | ✓ | ✓ | ✓ |
| disjoint-history guard (BEH-597) | ✓ | ✗ | ✗ |
| post-rebase collapse check (BEH-680) | `verify.PostRebasePush` | raw `BranchDiffEmpty` | ✗ |
| conflict | resolve-and-regate session | abort | abort |
| push + PR | `ship.Finish` | `ship.Finish` | force-push only |

The review tail also mapped outcomes onto a `Result` at eight separate return sites,
and each test of a landing edge needed its own Fake subclass (`collapsing`,
`emptyAfterRebase`).

## Decision

**`ship` owns the whole landing and returns one verdict.**

```go
func Land(h Port, log EventSink, slug string, t ticket.Ticket, resolve Resolve) Landing
// refetch → replay → disjoint guard → resolve hook → collapse check → push → PR

type Verdict int // Landed · Collapsed · Conflict · Disjoint · PushFailed · PRFailed

func Replay(h Port, slug string) Replayed // ReplayClean · ReplayConflict · ReplayDisjoint
```

- The **review stage** passes its resolve-and-regate session as the `Resolve`
  hook and maps the verdict onto a Disposition in one `switch`.
- **`Recover`** passes no hook, so a conflict aborts, and it maps each verdict
  onto a narration line.
- The **CI watch** reuses `Replay`, the rebase half, and then force-pushes.

The fetch stays outside `Replay` because the callers disagree on what a failed
fetch means. A landing rebases onto the ref it already has. The CI watch refuses to
re-push a branch replayed onto a base it could not refresh.

`ship` declares the host slice it needs as a `Port` (embedding
`verify.GroundTruth`), as ADR-0016 did for `verify`. It does not take
`hostio.Host`. That is what lets `hostio`'s CI watch call `ship.Replay` without an
import cycle. `hostio` asserts `var _ ship.Port = (Host)(nil)`.

## Consequences

- Guards are fixed once. The recovery and the CI watch gain the disjoint-history
  guard, and the recovery's collapse check goes through `verify.PostRebasePush`
  like the review's.
- `ship.Finish` and `ship.Outcome` are gone. `finish` is unexported, and its
  cumulative `Pushed`/`URL` markers became the `PushFailed`/`PRFailed` verdicts.
- One verdict-table test (`TestLandVerdicts`) replaces the scattered branches.
  `hostio.Fake.CollapseOnRebase` replaces both hand-rolled collapse subclasses.
- `ship`'s tests moved to the external `ship_test` package, because they use
  `hostio.Fake` and `hostio` now imports `ship`.
- A landing refetches origin/main even after the recovery just fetched. That
  costs one extra fetch per recovery, in exchange for one sequence.
- The review stage's `Replay` now calls `AbortRebase` before handing a conflict
  to its resolution session. Git's replay has already restored the branch, so
  the call is a no-op.
