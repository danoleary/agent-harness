# ADR-0069: CI still pending past the poll budget is unknown, not red

- Status: Accepted
- Date: 2026-11-22
- Extends: ADR-0019 (a loop is a Source and a Pipeline)

## Context

The CI watch polls a PR's checks until they reach a terminal verdict, bounded by
`CI_POLL_BUDGET_MS` and its adaptive extension up to `CI_POLL_MAX_BUDGET_MS`. A
CI run that is still going when the budget runs out ends the watch with
`ErrPollTimeout`. That outcome was not OK and carried no class of its own, so
the PR loop narrated it as "✗ CI did not go green", the same line as a red CI
after the auto-fix budget.

In #421 an auto-fix passed every local gate on the branch and the merge ref, was
pushed, and was polled for 25 minutes on queued self-hosted runners without
settling. The CI result was unknown, yet the run log read as a failure. A later
loop pass, or a reader of the log, could not tell that run from a real red one.
A watch that timed out before it pushed anything had launched no session and
changed nothing on the branch, but it was a **NoPR** run: it spent the PR's
attempt and counted against the circuit breaker, so a Consumer whose CI graph
routinely outlasts the budget could trip the breaker on waiting alone.

## Decision

1. **The poll timeout is its own outcome.** `ci.Outcome.Pending` marks it,
   apart from a red CI and from a poll fault (#383). Only `ErrPollTimeout` sets
   it. A stalled poll (`ErrPollStalled`) keeps its own reason: it says a
   required check may be wedged, which is for a human to look at.
2. **The PR and security-alerts loops narrate it as pending.** The line is
   "⧗ CI still pending past the poll budget (not a red CI) on <checks>", and it
   names the two knobs, never "CI did not go green".
3. **A PR run that pushed nothing before the timeout is CIPending.** It is a new
   **Disposition**. The Source takes the attempt back with a void marker and a
   comment on the PR, the loop releases the PR's label with no no-PR breadcrumb,
   and the breaker stays blind to it. The next poll re-checks that commit's CI
   before it works the PR again: it skips the PR while CI is pending or once it is
   green, and works it, as a fresh attempt, only once CI is red. A run that
   pushed a fix before the timeout stays **Shipped**, whatever its CI has yet to
   say. The next poll judges the new head the same way.
4. **The poll budget stays an env knob.** `CI_POLL_BUDGET_MS` and
   `CI_POLL_MAX_BUDGET_MS` are read from the Consumer's own `.agent-harness/.env`,
   so a Consumer with a long CI graph already raises them without a new
   `config.toml` key. The narration and the error catalogue point at them.
5. **The pending line names the budget the watch used** (#424). The poll's
   timeout carries how long it polled and both budgets, and the narration
   appends `after <d> of polling (CI_POLL_BUDGET_MS=<d>, CI_POLL_MAX_BUDGET_MS=<d>)`.
   An operator then sees whether the Consumer's own values or the defaults
   applied, and how far to raise them. The 25m default ceiling stays: it is
   kept under the 30m `CI_FIX_BUDGET_MS`, so the extension cannot outlive the
   whole watch and fix loop. A Consumer whose CI graph takes longer raises
   both knobs.

## Consequences

- A slow or queued CI no longer reads as a red one, and no fix session is
  launched for a commit whose CI has not failed.
- A PR whose CI keeps a failed check while the rest of its graph runs is
  selected red, waits out the budget and is taken back, poll after poll, until
  CI settles. Each wait is bounded by the poll budget and launches no session.
- The `Disposition` set grows by one. The ticket loop never produces it.
