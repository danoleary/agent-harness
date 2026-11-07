# ADR-0042: The committed-fix recovery lands only a tip the review cleared

- Status: Accepted
- Date: 2026-11-07
- Amends: ADR-0017 (`Recover` takes the review's push gate), ADR-0030 (adds a Disposition, NeedsHuman)

## Context

The committed-fix recovery (`ship.Recover`, BEH-713) finishes a no-PR run whose
branch is committed and unpushed. Its doc comment said it finished "a gate-green,
clean commit". The code checked only four things: the worktree exists, the tree
is clean, the branch has commits ahead of `origin/main`, and no PR is open. It did
not read the review verdict or the Gate results.

A review that refuses to ship leaves exactly that state. In herd on 2026-11-07,
BEH-1305's review wrote `Disposition: blocked` (only a human can regenerate
`database.types.ts`) and its `unit` Gate was red. BEH-1198's review stopped on a
red `storybook` Gate. In both cases the recovery pushed the branch about a
minute later and opened a PR. Both PRs went to pr-doctor as red CI, which
pr-doctor cannot fix when the fix needs a human.

## Decision

**The review stage records its push gate, and the recovery lands only a tip that
gate cleared.**

- **The record.** The review stage already kept its verdict and the HEAD it
  covered in the ticket's log dir (`review-verdict.json`, #44). The same record
  now also holds the last host Gate re-run over that HEAD (its exit code and the
  Gate that went red) and whether the verdict was blocked. The review writes it
  whenever the tree is clean, with or without a verdict. A review that stops
  short of its verdict over green Gates is the BEH-713 case, and the recovery
  still has to see it. A run that emits no verdict over the same HEAD as an
  earlier run keeps that earlier verdict. `stages.LastPushGate` reads the record
  back as a `ship.PushGate`.
- **The recovery** (`ship.Recover(h, log, key, gate)`) keeps its four checks and
  adds one. If no recorded Gate re-run covers the current tip, it declines
  (`Attempted=false`). Examples: a checkpoint commit made after the review, or a
  record from before this change. The ticket is then released as before, so a
  later run reviews the tip. If the record covers the tip and is blocked or red,
  the recovery refuses: `Attempted=true, Shipped=false`, and `Refused` gives the
  reason. Nothing is pushed and the worktree is kept. Only a covered tip with
  green Gates and no blocked verdict goes on to `Land`.
- **A new Disposition, NeedsHuman.** The ticket Pipeline maps a refusal onto
  `NeedsHuman`, with the reason in `Result.Refusal`. The loop does not release
  that ticket. Releasing it would re-run it to the same refusal. Instead it
  labels the ticket blocked through a new `Source.Block`. For tickets that is a
  new Tracker method, `MarkBlocked`, on all three adapters, using
  `tracker.blocked_label`. It also comments the reason and keeps the claim. If
  the label fails (for example, no `blocked_label` is configured), the failure is
  narrated and the comment asks the human to apply the label by hand. The breaker
  counts NeedsHuman as a failure to ship, which is what the run was.

## Consequences

- A blocked verdict or a red Gate now reaches a human as a labelled ticket with
  the reason on it, not as a red PR pr-doctor cannot fix.
- The recovery is stricter than before. A committed tip that no Gate re-run
  covered used to be pushed. It is now released, so it gets a review. That
  includes worktrees left by a harness from before this change, whose record has
  no Gate.
- An environmental Gate failure (the container died) is red like any other, so
  the ticket goes to a human rather than to a retry. The review stage already
  treats it as red and refuses to push.
- `blocked_label` now applies to Jira too. Jira's queue is `ready_jql`, so the
  Consumer has to exclude the label there.
- The stale-claim reaper does not read labels. A blocked ticket with no remote
  branch is still returned to Todo once its claim TTL passes, and from then on
  only the blocked label keeps selection off it.
