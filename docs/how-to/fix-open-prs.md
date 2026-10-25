# Fix open PRs with `pr-doctor`

A PR can go red on CI, or start conflicting with `main`, after its review stage
has finished. The built-in `pr-doctor` loop is a second daemon that fixes those
PRs. It runs beside the ticket loop (ADR-0019). It replaces the `loop --prs` flag
of earlier releases.

## Run it

```bash
agent-harness loop pr-doctor --dry-run   # list the PRs it would work, and how; changes nothing
agent-harness loop pr-doctor             # run it in the foreground
agent-harness start pr-doctor            # or as a detached daemon (loop-pr-doctor.pid, loop-pr-doctor.log)
touch "$PROJECT_PATH/.agent-harness/STOP-pr-doctor"   # wind it down
```

It needs nothing beyond what the ticket loop needs: `gh` and `GH_TOKEN` on the
host. Run one PR loop per checkout. It logs to
`.agent-harness/logs/loops/pr-doctor/`, one dir per PR plus its own `loop.jsonl`;
follow it with `agent-harness watch --loop pr-doctor`.

## Choose which PRs it touches

A PR qualifies if its head branch is on your `branch_prefix` (the harness opened
it), or if a human added the opt-in label. The label is `agent-maintained` by
default; set the loop's `source.label` ([declare a loop](declare-a-loop.md)) or
`PR_LOOP_LABEL` to change it. It never works:

- a PR from a fork, because it cannot push there;
- a PR into any base other than `main`;
- a PR labelled `blocked`;
- a PR whose branch is already being worked, either because the PR has the
  `in-progress` label or because the ticket loop holds that branch's ticket In
  Progress;
- a PR failing in a way its `source.when` leaves out.

When your tracker is GitHub Issues, the loop uses your `in_progress_label` and
`blocked_label` names instead of `in-progress` and `blocked`.

## What it does to a conflict

The strategy depends on whose branch it is:

| Branch | Strategy | Push |
|---|---|---|
| On your `branch_prefix` (the harness's) | rebase onto `origin/main` | `--force-with-lease` |
| Opted in with the label (a human's) | merge `origin/main` into the branch | plain push, never forced |

A human's branch is never rewritten, because someone else may have it checked
out. For the same reason, the CI watch does not reactively rebase a human's
branch.

## What it does to red CI

The loop re-runs the failed checks once, to rule out a flake. If they are still
red, it runs a CIFix session over the failing logs. The fix must be committed and
every Gate must pass before anything is pushed. Pushes to a PR only append
commits; the loop never opens a new PR.

Every fix is checked host-side before it is pushed: the conflict session must
leave a clean tree with `origin/main` in its history, a CI fix must be committed,
and every Gate must be green. If any check fails, nothing is pushed. After a push,
the loop watches CI until it passes or fails, then runs the retrospective over the
session transcripts. Project findings from a PR run are filed only when your
tracker is GitHub Issues. Harness findings are always routed.

## Give a PR a fresh attempt budget

The loop works each PR once per head commit and failure kind, and records each
attempt in a hidden marker comment on the PR, so a restarted daemon neither
retries a commit nor loses count. Any new push starts a new attempt. After
`source.max_attempts` attempts (else `PR_LOOP_MAX_ATTEMPTS`, default 3), the loop
comments on the PR with what it tried and labels it `blocked`. Remove the label to
give it a fresh budget.
