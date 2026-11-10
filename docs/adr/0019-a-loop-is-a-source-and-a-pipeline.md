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
     failure kind). The loop never retries a head SHA it has already tried (reversed
     for an attempt that pushed nothing: see the 2026-11-01 amendment). A new
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
  daemon's own items, so this is safe with one PR loop per checkout. A held PR
  with no attempt marker is not the loop's claim and is never reaped: a human
  set that label, or another loop holds the PR under it (#196).
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

## Amendment, 2026-10-25: each loop's daemon has its own pidfile, and the viewer follows a loop by name (#106)

Step (c) gave each loop its own sentinel and stream, but not its own pidfile, and
the viewer found the pidfile and `STOP` one directory above the `logs/` it
tailed. That holds for the ticket loop's `logs/loop.jsonl`, but for a loop's
`logs/loops/<name>/loop.jsonl` it pointed at `logs/loops/loop.pid` and
`logs/loops/STOP`, which nothing writes. A viewer following `pr-doctor` showed
the daemon down and never showed it winding down.

- **`agent-harness start <name>` launches any loop**, and its daemon records its
  pid in `loop-<name>.pid` beside `loop.pid` (`config.Loop.PidFile`). `tickets`
  keeps `loop.pid`; an instance keeps `loop-<id>.pid`. An id may not be a loop's
  name, so no two daemons share a pidfile. The daemon's startup check for another
  live daemon already globs `loop-*.pid`, so a running `pr-doctor` keeps the
  shared sentinels from being cleared under it.
- **`agent-harness watch --loop <name>`** follows a loop by the names the daemon
  uses (`config.Loop`'s `LogsDir`, `PidFile`, `StopFile`), and is winding down on
  any sentinel that stops it: an instance's `STOP-<id>`, the loop's `STOP-<name>`,
  and the plain `STOP`. A stream named by path is matched to its daemon by where
  it sits under `logs/`, so the path form probes the same files.
- A loop run in the foreground (`agent-harness loop <name>`) writes no pidfile,
  so the viewer reads it as down, as it always has for the ticket loop.

## Amendment, 2026-11-01: an attempt that pushed nothing is retried, and a gate that crashed is re-run once (#167)

On herd PR #1312 a CIFix session fixed the red `Build`, and then the host's
`storybook` gate went red over the fix because the test browser's connection
closed under a story the fix never touched. The harness did not push, and the fix
was lost with the worktree. The head did not move, so every later poll skipped the
PR as "already tried — waiting for a new push". It had used 1 of its 3 attempts, so
it was not handed off either. It sat red with no visible state.

- **An attempt at the PR's current head pushed nothing**, because a push moves the
  head. Such a marker no longer skips the PR. The PR is worked again, and the
  attempt counts against the budget, so a PR that keeps failing reaches the hand-off
  after `max_attempts`. The attempt comment and the dry-run say it is a retry. An
  attempt a spending cap cut short pushed nothing too, so it also spends one.
- **The attempt that spends the budget hands off when its lease is released.** If
  the PR is still failing at the head that attempt tried, the Source comments and
  labels it `blocked` then, not on the next poll. A PR that fails its whole budget in
  a row is that many consecutive no-push runs, which trips the daemon's breaker at
  the default limits (3 and 3), and the hand-off must not wait for a restart. A
  loop whose `max_consecutive_failures` is above its `max_attempts` keeps running.
- **A red gate with an infrastructure signature is re-run once** before it counts
  (`fix.Env.Gates`, so the review stage gets it too): the test browser's connection
  closing, an out-of-memory kill or SIGKILL, a network failure downloading a
  toolchain or package, or the gate cap. The signatures are read from the gate's
  step log and stderr tail. An exit 137 the OOM-retry schedule (BEH-530) already
  spent its attempts on is not re-run again.
- **A CI fix's gate that is red only in files the fix did not touch is re-run once
  too** (`fix.Env.GatesOver`, over the files the fix's commits changed). A failure
  in a file the fix changed is final.
- The re-run is one more gate run, and only its exit counts, so the gate's exit is
  still the only thing that authorises a push (ADR-0016). The signals lean towards
  re-running: a red the diff caused is red again, at the cost of one gate run.

## Amendment, 2026-11-01: the Source words the daemon's release lines (#173)

On herd PR-1312 (#167) an attempt pushed nothing, and the PR loop's stream said
`PR-1312 produced no PR; releasing back to Todo so it isn't stranded In
Progress`. `Source.Comment` already kept the ticket-worded breadcrumbs off PRs,
but the daemon's own stream lines were still written for tickets.

- **`Source.Releasing(why, key)` words the line the daemon writes as it gives an
  item back**, for each of the three reasons: a run that shipped nothing, a
  spending-cap abort, and a stale claim the reaper releases. The daemon adds the
  `loop — ` prefix and the record kind, and still owns when to release.
- The ticket Source keeps the wording it had ("releasing back to Todo"). The PR
  Source says what happens to a PR: `PR-1312 pushed nothing; releasing its
  in-progress label`, naming the lease label it is configured with.
- The breadcrumb bodies stay in the daemon. Only the ticket Source posts them.

## Amendment, 2026-11-07: a branch another worktree has checked out is skipped, not attempted (#192)

On herd PR #1375 a Claude Code worktree had `feat/beh-1298` checked out. git
refuses to check a branch out in a second worktree, so `checkout_pr` failed in
seconds, three times in 30 seconds. Each attempt had already been recorded when
the PR was selected, so the PR spent its whole budget and was labelled `blocked`
before any session ran. It stayed blocked after the worktree was removed.

- **The Source reads `git worktree list --porcelain` on every poll** and skips a
  PR whose head branch a worktree other than the loop's own `pr-<n>` has checked
  out: `branch checked out in worktree <path>`. Like the ticket loop's hold, the
  skip comes before any GitHub read, takes no lease, records no attempt and hands
  nothing off, so `--dry-run` shows it and the budget is untouched. The PR is
  judged afresh on the first poll after the worktree is gone.
- **The main checkout counts as a worktree.** git refuses its branch the same
  way, and so does a prunable worktree's until it is pruned.
- **The loop never removes or reuses a worktree it did not create.** Someone may
  be working in it. (Superseded by ADR-0043: a clean worktree that holds nothing
  origin lacks, with no live run in it, is removed; any other is reported in the
  stream.)
- A failed worktree list fails the poll, as a failed read of the ticket loop's
  holds does: a guess would spend an attempt on a checkout git refuses.

## Amendment, 2026-11-07: a third Source, open security alerts (#197)

ADR-0040 adds a third loop instance, the `github_security_alerts` Source. It is
the first loop whose Work item has neither a ticket nor a PR. Its Pipeline cuts
a fresh branch from `origin/main`, lands it as a new PR with `ship`, and keys its
lease on that branch as decision 2 requires: the claim lock until the PR exists,
then the `in-progress` label on the PR.

## Amendment, 2026-11-07: a checkout refused on local state is not an attempt (#247)

On herd PR-1384 the first fix attempt left its worktree dirty, and the loop kept
it for a look. The next two selections each recorded an attempt, then failed in
seconds in `CheckoutRemote`, because git refused to drop that worktree. Neither
run launched a session, but both counted against the PR's budget and the
breaker's streak. The breaker reported `PR-1384 failed 3×` and stopped the loop,
so no other PR got service. The #192 skip did not apply: the worktree was the
loop's own.

- **`CheckoutRemote` reports a refusal after the fetch as a
  `git.LocalStateError`** naming the worktree and carrying git's error: a dirty
  or locked worktree, the branch held by another worktree, a path in the way. A
  fetch that fails after its retries is the remote's failure and stays **NoPR**.
- **A new Disposition, CheckoutBlocked.** The PR Pipeline returns it on a
  local-state refusal. Like **PreflightAborted** it is environmental: no session
  ran, so the breaker stays blind to it. The loop narrates the PR and the error
  and backs off one poll.
- **The attempt is taken back.** Before releasing the lease, the Pipeline has
  the Source post a `void` marker for the head and kind, which cancels the
  latest matching attempt marker, so the budget counts only attempts that got
  past the checkout.
- **The PR is handed to a human.** Retrying would meet the same refusal, and the
  loop never discards a worktree with uncommitted work. The Source labels the PR
  `blocked` and comments with the worktree and git's error. This is not the
  budget hand-off, so it does not reset the budget. Once someone cleans the
  worktree up and removes the label, the PR resumes with the attempts it had
  left.

## Amendment, 2026-11-07: a preflight abort is not an attempt (#232)

In herd, a base-image bump dropped `corepack`, so the sandbox image would not
build and every run ended in a Docker preflight abort. The PR Source had
already posted its attempt marker at selection, so herd#1384 and herd#1385 each
went from attempt 1 to `blocked` in about three minutes without running a
session. The loop's record said `environmental: full disk / daemon down` and
dropped the build error.

- **The attempt is taken back.** On **PreflightAborted** the PR Pipeline has the
  Source post the same `void` marker a refused checkout gets, before the lease
  is released, so the budget counts only attempts that got past preflight. The
  comment carries the preflight error. Unlike a refused checkout, the PR is not
  labelled `blocked`: the fault is the host's, and the PR is worked again once
  the sandbox starts. The ticket loop keeps no attempt budget (the
  implementation stage releases the ticket to Todo), so it needs no change.
- **The record carries the real error.** The `ticket-released` record for a
  preflight abort puts the preflight's error in `detail` instead of guessing at
  the cause.
- **Aborts in a row back off longer.** The first waits one `poll_interval`, and
  each abort after it doubles the wait, up to 16 poll intervals. Any other
  outcome resets it. The loop counts aborts in a row rather than comparing
  errors, because a failed build's output carries step timings.

## Amendment, 2026-11-08: a run's uncommitted work is saved as a checkpoint, and the retry resumes from it (#246)

The #247 amendment stopped refused checkouts from spending attempts, but on
herd PR-1384 it still left one real attempt and a PR labelled `blocked`. CI-fix
session 1 wrote a fix, never committed it, and the 25-minute cap killed it. The
run kept the dirty worktree for a look, so the next checkout had to fail. The
ticket loop already saves such work as a harness recovery checkpoint
(`git.Worktree.Checkpoint`, BEH-479/BEH-559). The PR loop now does too.

- **A run ends with no dirty worktree.** When a PR run ends and its worktree
  holds uncommitted work, whichever session left it, the Pipeline commits that
  work as a recovery checkpoint (`checkpoint(harness): …`) on the local branch.
  It pushes nothing. The worktree is then clean, so it is removed as usual.
- **The retry at the same head starts from the checkpoint.** `CheckoutRemote`
  reuses the local branch, instead of resetting it to origin's tip, only when
  its tip is a recovery checkpoint and origin's tip is behind it. Otherwise it
  resets the branch as before: a checkpoint origin has moved past was cut from a
  head that is gone, and an ordinary unpushed commit (a fix the gates refused)
  is no checkpoint. The polled-head check compares origin's tip, under the
  checkpoint, with the polled head.
- **The session is told the work is unverified.** The CIFix and RebaseFix
  prompts name the checkpoint and the tip under it, say that no gate has run
  over it and that it can be wrong, and give the reset that drops it. In the
  incident the saved diff was the wrong fix: a `waitFor` that hung the story
  runner. Whatever the session keeps still passes every gate before any push.
- **A checkpoint that cannot be committed goes to a human at once.** The next
  checkout would refuse the dirty worktree, so the run's result carries a
  `prloop.UnsavedError` naming the worktree, and the Source labels the PR
  `blocked` with a comment naming it. A session ran, so the attempt stays spent;
  the Disposition is unchanged. #247's refusal remains the backstop if that
  hand-off fails.

The security-alerts loop does not change: its checkout cuts a fresh branch and
deliberately drops a failed attempt's commits (ADR-0040), so it still keeps a
dirty worktree for a look.

## Amendment, 2026-11-10: a committed conflict resolution survives a red re-gate (#252)

A conflict-resolution session rebased and committed a branch cleanly, and the
re-gate over it went red for a reason outside the branch. The run logged
`keeping worktree, nothing pushed`, but the cleanup removed the clean worktree,
and the next run's checkout reset the branch to origin's tip (`worktree add -B`).
Only the reflog still had the resolution. The next run saw the same conflict at
the same head, and a second session redid it, taking twice as long.

- **A resolution the gates hold back is saved.** When the ground truth accepts a
  resolution and the re-gate refuses it, the run saves the tip as
  `refs/harness/resolved/<slug>/<head>`, keyed on the PR head it resolved: a
  rebase leaves no trace of that head in the commits. It pushes nothing. One
  slug keeps one saved resolution.
- **The next run at that head re-gates it instead of opening a session**, while
  origin/main as just fetched is still an ancestor of the tip (and not the tip
  itself). The run moves the fresh worktree onto the tip, runs every gate, and
  pushes as the strategy says: force-with-lease after a rebase, plain after a
  merge. A red gate keeps it saved and pushes nothing. No session ran, so no
  retrospective runs.
- **A stale resolution is dropped.** Once origin/main moves past its base it
  resolves a conflict the PR no longer has, so the run drops it and launches
  RebaseFix as before. A pushed resolution is dropped too. A run that resumed a
  recovery checkpoint (#246) resolves afresh from it.
- **The log says what happens to the worktree.** `fix.Env.ResolveConflict` no
  longer claims the worktree is kept; its caller says so (the review stage keeps
  it), and the PR loop's cleanup narrates removing a clean worktree.

## Amendment, 2026-11-10: a CI fix is gated merged with origin/main too (#259)

The CI watch gated a CIFix commit on the branch HEAD alone. The red check it
answers ran on the PR's merge ref, which is the branch merged with current
origin/main. A fix that was green alone and red once merged passed the gates and
was pushed, and CI went red again. That cost another fix attempt from the budget
and another CI round, for a failure the host could have found itself.

- **A fix green on the branch is gated again over a trial merge** when origin/main
  is not an ancestor of its HEAD (`fix.GatedCI`). The host refetches origin/main,
  then merges it into a detached scratch worktree at the fix's HEAD
  (`git merge --no-commit --no-ff`, `gitpkg.Worktree.TrialMerge`, at
  `.claude/worktrees/<slug>.merge`). It runs `post_create` and every gate there,
  then removes the scratch worktree. The branch and its worktree never gain
  origin/main's changes. The branch's own history, and any rebase, stay the
  harness's (decision 4).
- **A merge that conflicts refuses the fix.** No merged tree exists to gate. The
  PR reads as conflicting, so the next poll works it as a conflict.
- **A red merge gate refuses the fix without a push, and the next attempt starts
  at once.** The refusal wraps `ci.ErrFixRefused`. `ci.WatchAndFix` does not push or
  poll for it, and it counts against `CI_MAX_FIX_ATTEMPTS`. The next CIFix session
  gets the merge gate's output as its logs, in place of the CI log the refused fix
  already answered. This replaces waiting a CI round for CI to report the same red.
- The merge gate runs the plain gates. A red in a file the fix did not touch is
  not re-run as it is on the branch (#167): origin/main's changes are exactly what
  a merge can break. A docs-only branch is not merge-gated, as it is not gated.
