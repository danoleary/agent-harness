# ADR-0065: A session is told its role and whether it may write

- Status: Accepted
- Date: 2026-11-22
- Extends: ADR-0046 (a ticket's session starts in its worktree)

## Context

ADR-0046 starts every session over a worktree in that worktree, so Claude Code
loads the branch's `.claude/settings.json`, hooks included. A Consumer's hooks
are written for the session that hands off a commit. herd's Stop hooks check
that the handoff is formatted and warn about uncommitted files.

The retrospective starts in the same worktree and loads the same hooks (#391).
Its contract forbids code changes and commits, yet in one herd run its Stop hooks
reported the format check RED with an empty formatter tail, about 169
uncommitted files, and told the session to run the formatter and re-commit. The
session had made no edits. The worktree directory had gone while it ran, so the
hooks were reading a path that no longer existed. An obedient session is pushed
toward writing to a repository it must not touch, and a hook has no way to tell
which session it is in.

## Decision

1. **Every session is told its role.** The launcher exports
   `HARNESS_SESSION_ROLE` into the container: `implementation`, `review`,
   `ci-fix`, `rebase-fix`, `security-fix`, `retrospective` or `triage`
   (`sandbox.Role`, `hostio.AgentRun.Role`). The resolution review is a
   `review`.
2. **A session that must not write is told so.** The retrospective and triage
   also get `HARNESS_SESSION_READ_ONLY=1`. The other roles do not set it, so a
   hook can check one variable and need not track the list of roles.
3. **The hooks stay the Consumer's.** The harness does not disable or filter the
   Consumer's hooks in a read-only session. Its own `--settings` can add hooks
   but cannot remove the project's, and `disableAllHooks` would also drop the
   harness's PreToolUse guards and any safety hook the Consumer relies on. A
   Consumer's handoff hooks check the variable and exit 0. The env reference
   gives the guard, including the check that the worktree is still on disk,
   which a hook should make first and answer with `worktree missing`, not a red
   result over empty output.
4. **The worktree outlives its run's retrospective.** The ticket's retrospective
   reaps the worktree only after its session returns, and the run lock
   (ADR-0061) keeps the PR loop off the branch until the whole run, the
   retrospective included, has ended. No new mechanism is added for that.

## Consequences

- A Consumer whose Stop hooks read neither variable still runs them in the
  retrospective. The fix is one line in the hook, which the env reference and
  CONSUMER.md show.
- A new session kind has to pick a role, and a read-only one has to be listed in
  `sandbox.Role.ReadOnly`.
