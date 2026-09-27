# ADR-0016: verify gathers its own ground truth

- Status: Accepted
- Date: 2026-09-27

## Context

One concept — "did this Stage really do its job?" — was spread across three modules,
and the split put the coverage on the wrong side of every seam.

```
stages/review.go   gather   clean · gateExit · emptyDiff · completeness   0.0% covered
        ↓
verify.Review      decide   5-field struct, 5-branch truth table          100% covered
        ↓
stages/review.go   act      push · PR · recommend-close                   0.0% covered
```

`internal/verify` was 459 lines: 9 pure decision functions and **12 parameter
structs** — `GroundTruth`, `Result`, `RetrospectiveOutcome`, `RetrospectiveInputs`,
`ReviewOutcome`, `ReviewCompleteness`, `ReviewRetryInputs`, `ReviewRetryDecision`,
`RebaseResolutionOutcome`, `RebaseResolutionResult`, `WorktreeReapOutcome`,
`WorktreeReapResult`. All 100% covered.

The bugs were never there. They were in the boolean expressions that *filled* the
struct:

```go
result := verify.Review(verify.ReviewOutcome{
	GatesGreen: gateExit == 0, WorktreeClean: clean,
	ReviewComplete: completeness.Complete,
	ReviewBlocked: reviewOutcome.ReviewBlocked, EmptyDiff: emptyDiff,
})
```

and in the BEH-624 fixed-point loop above it, whose `for` condition re-evaluated a
*second* predicate (`verify.ReviewVerdictRetry`) over five variables the loop body had
just mutated, while the push verdict was computed once after the loop from a
partially-overlapping set. The predicates were exhaustively tested; the loop that
wired them together, and could wire them to different state, was not (ADR-0013
flagged exactly this).

Filling a struct positionally also made the *order* of the decision unobservable.
`WorktreeReap` documented that a gh round-trip must never be spent on an unpushed
branch, but the short-circuit that enforced it lived at the call site
(`PRExists: pushed && h.PRExists(slug)`), not in the decision that owned the rule.

### A wrong-way import

`internal/git` imported `internal/verify` so `Worktree.GroundTruth` could return
`verify.GroundTruth`. The low-level git module depended on the decision module's
type, and `internal/git` could not be used without `internal/verify` (ADR-0015 left
this as the one dependency it could not fix from its own side).

## Decision

**`internal/verify` declares the ground truth it needs as a port, and every decision
gathers its own.**

```go
type GroundTruth interface {
	WorktreeExists(slug string) bool
	WorktreeClean(slug string) bool
	CommitsAhead(slug string) int
	BranchDisjoint(slug string) bool
	BranchDiffEmpty(slug string) bool
	BranchExists(slug string) bool
	BranchPushed(slug string) bool
	IsRebased(slug string) bool
	PRExists(slug string) bool
}

func Tdd(g GroundTruth, slug string) Result
func Review(g GroundTruth, slug string, gateExit int, out session.Outcome) Result
func RebaseResolution(g GroundTruth, slug string, out session.Outcome) Result
func WorktreeReap(g GroundTruth, slug string) Result
```

A Stage passes the `hostio.Host` it already holds and acts on one `Result`. What the
Stage still supplies is only what is *not* git: the gate's exit code, the session
outcome, and the two filesystem reads (the findings dropbox, the prior transcripts).

Declaring the port in `verify` rather than returning a `verify` type from
`internal/git` is what reverses the import. `internal/git` keeps the reads as its own
methods — `Worktree.GroundTruth` splits into `CommitsAhead` and `DisjointHistory` —
and no longer knows `internal/verify` exists. `hostio` asserts the port it satisfies:

```go
var _ verify.GroundTruth = (Host)(nil)
```

so adding a read to the decision layer without adding it to the seam stops compiling.

The 11 parameter structs collapse into the implementation, leaving one `Result`.
Two of the nine decision functions — `ReviewQualitative` and `ReviewVerdictRetry` —
become unexported and are reported on `Review`'s `Result`, because both answer a
question about the *same* review over the *same* ground truth. The fixed-point loop
then re-decides through one call:

```go
result := verify.Review(h, slug, gateExit, reviewOutcome)
for attempt := 2; attempt <= reviewVerdictMaxAttempts && result.Retry; attempt++ {
	reviewOutcome = runReviewSession(attempt)
	gateRes = runHostGate(fmt.Sprintf("%s-retry%d", gateStep, attempt))
	gateExit = gateRes.Outcome.ExitCode
	result = verify.Review(h, slug, gateExit, reviewOutcome)
}
```

The loop's condition and the push verdict are now the same value, so they cannot be
computed from different state.

## Consequences

- Exported surface: **21 names → 9** (9 functions + 12 structs → 7 functions + 2
  types). `internal/verify` 459 → 438 lines; `review.go` 665 → 641,
  `implementation.go` 510 → 488, `retrospective.go` 218 → 207.
- `internal/git` no longer imports `internal/verify`. The dependency runs
  git → (nothing), verify → git's reads via the port, hostio → both.
- `verify` stays at **100%** statement coverage, and the tests now exercise reads
  rather than hand-written structs: a scripted `GroundTruth` records *which* reads a
  decision spent, so the orderings the doc comments promise — disjoint checked only
  once a branch is ahead, empty-diff only over a clean tree, `PRExists` only for a
  pushed branch, a spending-cap abort settling before any git read — are asserted
  instead of described. The gather itself is pinned against real git one layer down
  (`CommitsAhead`, `DisjointHistory`, `Exists` and the rest are 100% covered over
  temp repos in `internal/git`), and the composition runs through `hostio.Fake` in
  `internal/stages`.
- One behaviour becomes consistent rather than scripted. The BEH-389
  usage-policy-refusal retry is gated on a surviving worktree, and that gate now reads
  the worktree live, as it always did in production — `hostio.Fake` was the only place
  a stale snapshot could disagree with it. Since BEH-636 pre-creates the worktree
  host-side and a creation failure is fatal upstream, the gate is defence-in-depth
  against a worktree vanishing mid-run, and its test says so.
- `internal/stages`' own statement coverage reads 79.6% → 79.1%. Nothing became
  unreachable: the lines that left were covered ones — the struct literals and the
  `disjointWorkTrapped` helper, whose case moved to `Result.DisjointWorkTrapped` and
  is tested in `verify`.
- `Result` now carries five decision-specific fields beside `OK`/`Reason`
  (`RecommendClose`, `SpendingCapAbort`, `Retry`, the two review-completeness fields,
  `DisjointWorkTrapped`). Each names the decision that sets it and is the zero value
  everywhere else — the cost of one verdict type instead of five, paid in doc comments
  rather than in call-site plumbing.
