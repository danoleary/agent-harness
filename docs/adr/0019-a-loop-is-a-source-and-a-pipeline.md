# ADR-0019: A loop is a Source and a Pipeline

- Status: Proposed
- Date: 2026-10-24

## Context

The harness has one loop. `loop.Run` (ADR-0004, ADR-0014) selects a tracker
ticket, runs implementation → review → retrospective over it, and repeats. Its
`loop.Host` combines three concerns that change at different rates:

| Concern | `loop.Host` methods | Specific to tickets? |
|---|---|---|
| **Source**: what work exists, and the claim on it | `ResolveNext`, `ReleaseTicket`, `CommentTicket`, `ListInProgressClaims`, `TicketHasRemoteBranch` | Yes: a tracker queue, keyed on a tracker **Key** |
| **Pipeline**: what happens to one item | `RunPipeline`, `RecoverCommittedFix` | Yes: the three ticket Stages |
| **Daemon chores** | `ClearStopFile`, `CheckAccess`, `StopRequested`, `FetchMain`, `FreeDisk`, the three prunes; plus `Limits` and `Clock` | No |

The daemon chores are the part that took the most work to get right: stop
control, idle re-poll, spending-cap backoff, the circuit breaker, disk reclaim.
None of them depends on tickets. The Source and the Pipeline do.

We want a second loop: one that polls GitHub for open PRs that have a merge
conflict or failing CI, and fixes each in a fresh sandboxed session. Most of
that loop already exists inside the review stage, but it is only reachable on a
branch the review stage has just created:

- **Failing CI**: `ci.WatchAndFix` with `ciFixRunner` (`internal/stages/review.go`).
  It fetches the failing logs, runs a `CIFix` session over the worktree, checks
  that the fix was committed, pushes, and polls again.
- **Merge conflict**: `resolvePrePushConflict` with the `RebaseFix` prompt, then
  `verify.RebaseResolution`, then the gates again.
- **Mergeability**: `ci.ClassifyMergeState` (`internal/ci/merge.go`).
- **Push and land**: `ship.Land` and `Worktree.PushForceWithLease` (ADR-0017).

Consumers also want loops of their own, configured rather than coded.

## Decision

**A loop is a Source plus a Pipeline, run by the shared daemon.**

```
Loop      = Source + Pipeline + Limits      // Clock, Narrator and the daemon chores stay shared
Source    : Next() (WorkItem, Lease, ok, err); List() for the reaper; Release, Comment
WorkItem  : {Key, Branch, Context}          // a ticket, or a PR (number, head SHA, failure kind)
Pipeline  : Run(WorkItem, Lease) stages.Result
```

1. **Split `loop.Host`.** The daemon chores stay on `loop.Host`. The ticket queue
   becomes the first `Source`. The ticket pipeline becomes the first `Pipeline`.
   `loop.Run` takes one of each. The ticket loop keeps its current behaviour,
   tests and `Disposition`s, and the circuit breaker still counts `Disposition`s.
2. **The lease is keyed on the branch, not the ticket.** Two loops can reach the
   same branch: a ticket loop's open PR can go red while the PR loop is polling.
   The lease names the branch the work will push, so the second loop skips a
   branch the first one holds. A ticket lease still moves the tracker state. A PR
   lease is held with a label on the PR.
3. **The PR loop is the second instance, written in Go first.** It has three parts:
   - *Source* `github_prs`: open PRs that are conflicting or have red checks, and
     that it is allowed to touch (see 4). One item per (PR number, head SHA,
     failure kind). The loop never retries a head SHA it has already tried. A new
     push by anyone starts a new item. Each PR has an attempt budget; past it, the
     loop leaves a comment and labels the PR `blocked`.
   - *Pipeline*: check out the PR branch into a worktree → a fresh sandbox session
     with the `RebaseFix` or `CIFix` envelope plus the Consumer's body → the same
     ground-truth checks those sessions use today → the gates → push to the
     existing branch (no new PR) → `ci.WatchAndFix`. The retrospective runs last
     and files findings, as in the ticket pipeline.
   - It reuses the functions listed above, moved out of `internal/stages/review.go`
     so both pipelines call them. It does not copy them.
4. **The PR loop touches only PRs that opt in.** It works PRs the harness opened
   (on its `branch_prefix`) and PRs a human has labelled for it. It never works a
   PR from a fork, because it cannot push there. On a branch the harness opened, it
   resolves conflicts by rebasing and pushes with force-with-lease, as it does
   today. On a branch a human opted in, it merges the base branch into the PR
   branch and pushes normally, because a force-push rewrites history someone else
   may have checked out.
5. **Config chooses and wires Go Steps. It does not script behaviour.** Once there
   are two loops, `.agent-harness/config.toml` gets a `[[loops]]` table. Each loop
   names a Source kind with its parameters, an ordered list of Steps from a fixed
   set of names, and per-loop limits. Run one with `agent-harness loop <name>`. The
   ticket loop is the built-in default, so a config without `[[loops]]` behaves as
   it does today.

   ```toml
   [[loops]]
   name = "pr-doctor"
   poll_interval = "10m"
   source = { kind = "github_prs", label = "agent-maintained", when = ["conflicting", "ci_failing"] }
   steps = [
     { use = "checkout_pr" },
     { use = "fix", prompt = "prompts/fix-pr.md" },
     { use = "gates" },
     { use = "push_existing" },
     { use = "watch_ci" },
     { use = "retro" },
   ]
   ```

   Every Step is Go code that owns its ground-truth check: a clean tree, a branch
   on its base, green gates. A Consumer can reorder Steps, leave them out, change
   their parameters and supply prompt bodies (ADR-0009). A Consumer cannot add a
   Step that does nothing but run a shell command or an agent with no check after
   it.

## Alternatives considered

- **A general workflow file in YAML or JSON** (steps of type `agent`, `shell`,
  `if`). This is the most flexible option. It would also let a Consumer build a
  pipeline that pushes what an agent reports, with no host-side check, which
  removes what ADR-0016 exists to guarantee. It would add a dependency and a second
  config format next to `config.toml` (ADR-0008), in a module that has no
  dependencies on purpose. Rejected.
- **Write `[[loops]]` config before the second loop exists.** Rejected. With one
  instance the config schema would be a guess. The PR loop is written by hand
  first, and the config shape is taken from what the two loops share.
- **Run the PR fixer from the ticket loop's idle time** (fix red PRs whenever the
  queue is empty). It needs no new seam, but the PR fixer would never run while
  the queue has work, which is when PRs go red most often. Its claims and breaker
  counts would also mix with the ticket loop's. Rejected.
- **Use GitHub Actions** (a workflow on `check_suite` failure). It reacts faster,
  but the agent would run in CI with credentials. This repo keeps every credential
  on the host and never lets the sandbox reach a remote (ADR-0002).
  Rejected for now; a webhook-triggered Source could be added later behind the
  same `Source` port.
- **Key the lease on the PR number.** Rejected: a ticket's branch and its PR are
  the same work, and two keys for one branch is the race that decision 2 prevents.

## Consequences

- `loop.Host` gets smaller, and the Source and Pipeline each get a fake in
  `loop_test.go`, so the daemon is tested against a Source that is not a tracker.
- `ciFixRunner`, `resolvePrePushConflict` and the merge-state read move out of the
  review stage, which makes `review.go` smaller. #29 (one Stage frame) helps here
  but does not block it.
- A second loop doubles the number of daemons competing for Docker and disk. Disk
  reclaim stays per daemon. Two daemons pruning at once is safe because each prune
  checks a worktree is clean and its PR merged before removing it (ADR-0005).
- The stop sentinel and `loop.jsonl` become per loop (`.agent-harness/STOP` stays
  the ticket loop's for compatibility), so the viewer can follow either loop.
- New glossary terms for CONTEXT.md once the code lands: **Source**, **Work
  item**, **Step**.
- Sequencing, tracked as issues: (a) split `loop.Host` into Source, Pipeline and
  daemon, with no behaviour change; (b) the `github_prs` Source and PR-fixer
  Pipeline, wired by hand; (c) `[[loops]]` config and `agent-harness loop <name>`.
