# ADR-0019: A loop is a Source and a Pipeline

- Status: Accepted
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
  checks a worktree is clean and its PR merged before removing it (ADR-0027).
- The stop sentinel and `loop.jsonl` become per loop (`.agent-harness/STOP` stays
  the ticket loop's for compatibility), so the viewer can follow either loop.
- New glossary terms for CONTEXT.md once the code lands: **Source**, **Work
  item**, **Step**.
- Sequencing, tracked as issues: (a) split `loop.Host` into Source, Pipeline and
  daemon, with no behaviour change; (b) the `github_prs` Source and PR-fixer
  Pipeline, wired by hand; (c) `[[loops]]` config and `agent-harness loop <name>`.

## Step (b), as built

The PR loop shipped as `agent-harness loop --prs` (`internal/prloop`), with the
review stage's fix machinery moved to `internal/fix`: the gate re-run, the CIFix
callback and the RebaseFix resolution. Building it settled these points the
decision above left open:

- **What was tried is recorded in hidden marker comments on the PR.** Each attempt
  posts one, naming the head SHA and the failure kind. An existing marker for the
  same SHA and kind means the PR is skipped. The hand-off comment carries a marker
  too, and only attempts after the latest hand-off count against the budget. So a
  human who removes `blocked` grants a fresh budget.
- **Red CI goes through `ci.WatchAndFix` from the start.** The watch already
  fetches the failing logs, re-runs a flake once, and pushes only after `Fix`
  succeeds. The PR loop's `Fix` runs the CIFix session and then every gate, and
  refuses the fix when any gate is red. A flake that the re-run washes out
  launches no session and pushes nothing.
- **A human's branch is never rewritten, including by the CI watch.** Decision 4
  applies to the watch's reactive rebase as well: `CIWatch.KeepHistory` turns a
  green-but-conflicting human PR into a conflict outcome instead of a
  force-pushed replay.
- **Only PRs into `main` qualify.** The harness lands on one base everywhere else,
  and the ground truth (`origin/main` an ancestor of the tip) assumes it.
- **The ticket loop's hold on a branch** is read from the tracker's In Progress
  claims, each mapped to `<branch_prefix>/<slug>`. A PR's own lease is the
  `in-progress` label. With GitHub Issues as the tracker, the loop uses that
  tracker's label names, and the issue reaper now skips PRs so it cannot release a
  PR lease.
- **The daemon's breadcrumbs are not posted on PRs.** They are worded for tickets
  ("released back to Todo"), so `Source.Comment` is a no-op for PRs. The marker and
  hand-off comments are the record.
- **A PR lease left by a dead daemon is reaped** once it is older than the claim
  TTL, timed by the PR's latest attempt marker. The reaper runs only between a
  daemon's own items, so this is safe with one PR loop per checkout.
- **Findings.** The retrospective runs over the run's transcripts when a session
  ran. Project findings file only when the tracker is GitHub Issues, because a PR
  names no tracker team. Harness findings are always routed.
- **Knobs are environment variables until step (c):** `PR_LOOP_LABEL`,
  `PR_LOOP_MAX_ATTEMPTS` and `PR_LOOP_STOP_FILE`. They become `[[loops]]` source
  parameters there.

## Step (c), as built

`[[loops]]` shipped in `config.toml` (`internal/config/loops.go`), and `agent-harness
loop [<name>] [--list] [--dry-run]` replaced `--prs`, which is now refused with its
replacement rather than ignored. Building it settled these points:

- **Two loops are built in, and config replaces them by name.** `tickets` (source
  `tracker`, steps `implement, review, retro`) is what a bare `agent-harness loop`
  runs, and `pr-doctor` is the step (b) loop. A `[[loops]]` entry with either name
  replaces it; any other name adds a loop. Each built-in written out as an entry
  resolves to the same `config.Loop` the hard-coded wiring runs, and that is
  tested.
- **What config can change is what the two loops differ in.** A Source's
  parameters (`github_prs`: `label`, `when`, `max_attempts`; `tracker`: none, as
  `[tracker]` already describes the queue), which optional Steps run (`retro`,
  and `watch_ci` on a PR loop), a Step's prompt body, and the daemon limits.
  Each limit and Source parameter overrides its env knob for that loop only, and
  an unset one keeps the knob. `PR_LOOP_STOP_FILE` is gone: the sentinel follows
  the name.
- **The Step order is a fixed chain, checked at load.** Each Step names the one it
  must follow (`review` after `implement`; `fix` after `checkout_pr`; `gates` after
  `fix`; `push_existing` after `gates`; `watch_ci` after `push_existing`), `retro`
  must be last, and each Source has the Steps it cannot run without (`implement`,
  `review`; `checkout_pr`, `fix`, `gates`, `push_existing`). Steps run only on
  their own Source. Every problem is reported at once, at its `line:column`, which
  the loader gets by walking the file again with go-toml's own parser.
- **A red check is fixed inside `watch_ci`.** The CIFix session runs inside the CI
  watch, as in step (b), so `fix` is the conflict resolution and `when =
  ["ci_failing"]` needs `watch_ci`. The `fix` step's prompt body leads both the
  RebaseFix and the CIFix prompts, ahead of the envelope (ADR-0009). Without one,
  those prompts are unchanged.
- **The plain STOP now stops every loop**, which reverses the Consequences line
  above that kept it the ticket loop's. Each loop also has its own
  `.agent-harness/STOP-<name>`, and a starting loop clears both. A plain STOP that
  stopped one loop, while another ignored it, would surprise an operator more than
  one that stops them all.
- **Logs.** `tickets` keeps `.agent-harness/logs/`, so the viewer's default
  stream is unchanged. Every other loop logs under `logs/loops/<name>/`, so a loop
  name cannot collide with a ticket's log dir.
- **`--dry-run` prints the resolved loop** for any loop: its Source with the
  parameters in force, its Steps with what each checks, its limits, sentinel and
  stream. A PR loop also prints the PR plan. A ticket loop does not plan a ticket,
  because selecting one claims it; `pipeline --next --dry-run` already plans one.
