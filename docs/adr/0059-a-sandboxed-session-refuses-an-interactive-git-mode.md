# ADR-0059: A sandboxed session refuses an interactive git mode

- Status: Accepted
- Date: 2026-11-15
- Extends: ADR-0044 (adds a second PreToolUse hook to every session's `--settings`)
- Amended: 2026-11-24 (#433: the guard is installed as a script file, and its
  advice warns that checkout and restore drop every uncommitted change)

## Context

An implementation session ran a compound Bash command that included
`git checkout -p` (#353). Its hunk prompt waited on stdin, which the Bash tool
never answers. With stderr on `/dev/null` the session saw no prompt, and the
call sat until the sandbox's Bash default timeout, which the harness raised to
10 minutes for slow worktree provisioning. It then went to the background and
the session had to load TaskStop to kill it: 10 of its 30 active minutes, for no
output. The CLI's own system prompt warns only about `-i`, not `-p`/`--patch`,
and nothing enforced either.

## Decision

**Every sandboxed session carries a second PreToolUse hook on Bash that blocks a
git mode which prompts, before it runs.**

1. It travels in the same `--settings` JSON as the reaper (ADR-0044), so it
   reaches every Consumer's next session without a base-image release.
2. It blocks add, checkout, restore, reset, stash (bare, `push` or `save`) and
   commit with `-p`/`--patch`; add, clean and rebase with `-i`; and add, clean,
   rebase and commit with `--interactive`. `commit -i` is `--include` and passes,
   as do `diff`, `log`, `show` and `stash show` with `-p`, which print and never
   prompt.
3. It splits the command the way a shell would, near enough: quotes,
   backslashes, comments, heredoc bodies and `;&|()`. Only a word in command
   position (after assignments and `env`, `timeout N` and the like) counts as
   git, and a flag after `--` is a path. So a commit message, a grep pattern or
   an `echo` that names `git add -p` is not refused.
4. It exits 2 with a message naming the command and the non-interactive forms
   to use instead, so the session learns at once. A failure inside the hook
   lets the call through: the guard saves time, it is not a security boundary.
   POSIX sh with jq and awk, like the reaper.
5. It is a script file, not an inline hook command (#433). When a hook blocks,
   the CLI reports `PreToolUse:Bash hook error: [<the hook command>]: <stderr>`,
   so an inline guard handed the session 3 KB of awk before its one-line
   reason. A SessionStart hook in the same `--settings` writes the script to
   `/tmp/agent-harness/interactive-git-guard.sh` before the first tool call,
   and the PreToolUse hook is one line that runs it. A missing script lets the
   call through, as a failing awk does.
6. Its advice says `git checkout -- <path>` and `git restore <path>` discard
   every uncommitted change to the path, and that a temporary edit is undone by
   copying the file aside first and back, or by editing the file. A session
   reaches for `checkout -p` to drop only part of its work.

## Consequences

- A session cannot run `rebase -i` even with `GIT_SEQUENCE_EDITOR` set to a
  non-interactive editor. Nothing in a Stage needs it: sessions do not rebase.
- Other prompting tools still hang until the Bash timeout. Starting the
  session's Bash with stdin on `/dev/null` would cover them, but the pinned CLI has no
  flag for it.
- Every Bash call pays one more `jq` and `awk`.
- A session that deletes `/tmp/agent-harness` loses the guard until its next
  SessionStart (a resume or a compaction reinstalls it).
