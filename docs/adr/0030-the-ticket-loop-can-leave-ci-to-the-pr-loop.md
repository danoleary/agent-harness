# ADR-0030: The ticket loop can leave CI to the PR loop

- Status: Accepted
- Date: 2026-10-25
- Extends: ADR-0019

## Context

The review stage opens the PR and then watches its CI (`h.WatchCI`). It returns
only when CI is green, red past the auto-fix budget, or out of the poll budget.
The retrospective, the worktree reap and the next ticket wait for it. On one
Consumer's six runs on 2026-10-25 the watch took 9 to 16.5 minutes, 60–80% of the
review stage on most tickets, and every one went green (or never ran) without an
auto-fix session (#110).

The PR loop (ADR-0019) already polls open PRs on `branch_prefix` for red CI and
for a conflict with `main`. It runs the same CIFix session, re-gates, pushes and
watches CI to a verdict. Two things kept it from taking over a ticket's PR:

- Not every Consumer runs a PR loop. One that runs none would have red CI left
  unattended.
- The PR loop skips a branch the ticket loop holds, and it read the hold as
  "the branch's ticket is In Progress". A shipped ticket keeps its lease, so it
  stays In Progress (or another started state) until its PR merges. The PR loop
  never worked a harness PR whose ticket was still open.

## Decision

**`watch_ci` is an optional Step on a ticket loop too, and the ticket loop holds a
branch only while it has the branch checked out.**

1. **`watch_ci` on a `tracker` loop** follows `review`. It is the review stage's
   post-PR CI watch, which still runs inside the stage. The built-in `tickets`
   loop has it, so a Consumer that declares no `[[loops]]` keeps today's
   behaviour, and its red CI is still fixed.
2. **Without it**, the review stage returns `Shipped` (and OK) as soon as the PR
   is open, and never calls `WatchCI`. The retrospective runs at once, then reaps
   the worktree: it already does so once the branch is pushed and its PR exists.
   `config.Loop.LeavesCIToPRLoop` names the case. The ticket loop passes it to the
   stage in `stages.Args.LeaveCIToPRLoop`, and `pipeline` and `review` run by hand
   follow the Consumer's `tickets` loop.
3. **The hold** (`loophost.TicketSource.HeldBranches`) is an agent-claimed In
   Progress ticket whose worktree still exists in the checkout. That covers the
   whole run, including an in-stage CI watch and a kept worktree after red CI past
   the budget. It ends when the retrospective reaps the worktree, so a PR that goes
   red after the ticket loop moved on is the PR loop's `CIRed` item.

The breaker is unchanged: a pushed PR was already a success (DESIGN.md §Circuit
breaker).

## Alternatives considered

- **An env knob or a `[ci]` key** to switch the in-stage watch off. It would work,
  but the loop's Steps are where a Consumer already says what a loop does, and
  ADR-0019 already lets config leave out an optional Step. A knob would be a second
  place to look.
- **Turn the watch off by default and have `doctor` warn** when no PR loop is
  declared. `doctor` cannot tell whether the operator runs `pr-doctor`; a
  `[[loops]]` entry is not a running daemon. Leaving red CI unattended by default
  is the worse failure, so the watch stays on until a Consumer leaves it out.
- **Move the watch into its own Step after `review`**, run by the pipeline rather
  than the stage. It would let a retrospective run before the watch, but the watch
  needs the stage's worktree, ticket and conflict-resolution hook. No behaviour
  needs it yet.
- **Read the hold as "In Progress with no linked PR".** A ticket mid-watch has a
  linked PR, so the PR loop would race the review stage's own CIFix session.

## Consequences

- A Consumer that leaves out `watch_ci` must run a PR loop, or red CI on its PRs
  waits for a human. The how-to and the example config say so.
- The CI watch's merged-prerequisite resolver (#82, ADR-0023) does not run
  without `watch_ci`. The PR loop's conflict fix covers the same case: a harness
  PR that starts conflicting with `main` after a prerequisite merged is a
  `Conflict` item, rebased by a RebaseFix session and re-gated. It also covers a
  conflict that is not a prerequisite's, which the in-stage resolver left for a
  human.
- The CI watch's zero-net-diff short-circuit (BEH-602) does not run without
  `watch_ci` either. A branch that only became a no-op against `main` after the
  pre-push rebase ships as `Shipped` instead of `RecommendClose`. The PR stays
  open for a human to close as superseded. The pre-push `Collapsed` check
  (BEH-680) still catches the common case.
- A ticket loop that leaves out both `retro` and `watch_ci` never reaps its
  worktree, so its branch stays held and the PR loop skips the PR until the
  merged-worktree prune (ADR-0027) or a human removes it. The same holds when a
  retrospective fails and keeps the worktree.
- With or without `watch_ci`, the PR loop now works a harness PR once its
  ticket's run is over and the worktree is gone, where before it skipped every PR
  whose ticket was still In Progress.
