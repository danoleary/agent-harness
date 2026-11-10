# ADR-0046: A ticket's session starts in its worktree

- Status: Accepted
- Date: 2026-11-10
- Amends: ADR-0002 (the `-w $PROJECT_PATH` working directory)

## Context

ADR-0002 mounts the checkout at its real host path and starts every container
in it (`-v $PROJECT_PATH:$PROJECT_PATH -w $PROJECT_PATH`). The gate and
post_create containers were later moved to `-w <worktree>`, but the agent
sessions kept `-w $PROJECT_PATH` and were told, in a checkout-relative path,
to `cd` into `.claude/worktrees/<slug>`.

Claude Code reads `.claude/skills/`, `.claude/settings.json` and CLAUDE.md from
the directory it starts in. Started in the checkout, a session loads them as the
operator's checkout has them. The harness never updates that checkout, and it
can sit on any branch. In #245 it sat on a side branch without the base commit,
which had just reordered /tdd's Finish (commit before the full gate). The
implementation session loaded the older /tdd and ran the full gate before it
committed, and the review session loaded the older review skill. Nothing failed,
so nothing showed that a skill fix on main had not reached the run.

## Decision

1. **A session over a worktree starts in it.** The implementation, review,
   resolution-review and retrospective sessions, the PR loop's CI-fix and
   conflict-resolution sessions, and the SecurityFix session run with
   `-w <worktree>`, as the gates do (`hostio.AgentRun.WorkDir`,
   `sandbox.Config.WorkDir`). The skills, settings and CLAUDE.md they load are
   the branch's, which the harness cut from origin/main. The checkout stays
   mounted whole at its real path: the worktree's `.git` pointer resolves into
   it, and the logs, locks and findings live there.
2. **The envelope's paths are absolute.** The worktree path in the
   implementation envelope and the retrospective's log dir (`{{.LogDir}}`) are
   absolute, since the session no longer starts where a checkout-relative path
   resolves. The retrospective body names the worktree as `{{.WorktreePath}}`.
3. **A session left in the checkout says when its skills are stale.** Triage has
   no worktree, and a session whose worktree has gone from disk falls back to
   the checkout rather than have `docker run -w` create an empty directory. At
   launch, when the checkout's tracked `.claude/` files differ from origin/main,
   the run log narrates a warning naming them. `agent-harness doctor` reports
   the same drift as a warning, with the fix: update the checkout.

## Consequences

- A skill fix merged to main reaches the next ticket's sessions without anyone
  updating the host checkout.
- A branch that edits a skill runs its own sessions on its edited skill, as it
  already runs its gates on its own code. A broken skill edit fails on the
  branch that made it.
- Triage still loads the checkout's skills. The warning and the doctor line make
  that visible, but they do not fix it; the operator updates the checkout.
- A Consumer prompt body that names the worktree as `.claude/worktrees/{{.Slug}}`
  still renders. The path it names is relative to the checkout, not to where the
  session starts, so such a body should use `{{.WorktreePath}}`.
