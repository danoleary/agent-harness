# ADR-0057: The host runs no code the sandbox can write

- Status: Accepted
- Date: 2026-11-14
- Amends: ADR-0002 (host-side git is hardened against the checkout's own config),
  ADR-0008 (a run reads `config.toml` from origin/main, not the working tree)

## Context

The threat is prompt injection. An unattended session reads ticket text, fetched
documents and dependency READMEs, and any of them can steer it. Every container
bind-mounts the Consumer checkout read-write, `.git` included (ADR-0002), and the
host then ran code that a session could write (#332):

- **Host git ran the repository's hooks and config.** The host runs `git -C
  <checkout>` for `worktree add`, `fetch`, `reset --hard`, `cherry-pick`, `status`
  and `commit --no-verify`. `--no-verify` stops only `pre-commit` and
  `commit-msg`. A planted `.git/hooks/post-checkout` or `reference-transaction`, a
  `core.hooksPath` pointing elsewhere, or a `core.fsmonitor` command would run on
  the host, next to `GH_TOKEN` and the tracker key.
- **The host read `config.toml` from the working tree** on every ticket, and runs
  `[cache] prune_command` with `sh -c` on the host. A session could rewrite it.
- **The mask covered one file**, `.agent-harness/.env`. Any other `.env*` file in
  the checkout was readable in the sandbox.

## Decision

**Nothing the host executes or hands a credential to comes from a file a sandbox
can write.**

- **Every host-side `git` carries `-c core.hooksPath=/dev/null -c
  core.fsmonitor=false`.** A command-line `-c` beats every config file, the
  repository's `.git/config` included. The overrides live in `internal/git`'s
  production runners, so a call site added later gets them. A host-side git call
  outside that package's `Checkout` builds its argv with `git.HostArgs`.
  `Checkout.WithRepoHooks` drops them, for gittest's fault injection only.
- **A run loads `config.toml` from origin/main**: `git show
  origin/main:.agent-harness/config.toml` (`config.WithCommittedProject`, which
  `stages.LoadConfig` passes). Every command the config declares, `prune_command`,
  `post_create` and the Gates, comes from that copy, which only a merged PR
  changes. It reads the remote-tracking ref the last fetch left. A config in the
  working tree that was never pushed is an error that says to push it. `doctor`
  and `init` still read the working tree: the operator runs them, on the
  checkout they are editing. The loop's between-ticket
  `scripts/prune-merged-worktrees.sh` is likewise origin/main's copy, written to
  a temp file outside the checkout and run from there.
- **The mask is every `.env*` file in the checkout except `.env.example`.** The
  walk skips `.git` and `node_modules`, and masks only regular files it found, so
  Docker never creates a missing mount target in the checkout. It skips a file
  git tracks: its content is in the objects the sandbox reads anyway, and an
  empty mount over a committed `.env` would get the truncation committed by the
  session's `git add -A`.

## Alternatives considered

- **Read-only `.git` mount.** The session commits its handoff in the sandbox, so
  `.git` must be writable there.
- **`GIT_CONFIG_NOSYSTEM` or a scrubbed `HOME`.** Neither stops the repository's
  own `.git/config`, which is the file a session can write.
- **Glob the mask from config.** A Consumer would have to list every credential
  file, and one it forgot would leak. A deny-list by name is the default.

## Consequences

- A config change takes effect once it is merged to main and fetched, not when it
  is saved. An operator trying a new Gate pushes it first.
- A Consumer's own git hooks never run on the host's git, Git LFS's `pre-push`
  included. A Consumer that needs LFS objects uploaded on push is not served by
  host push today.
- Not closed here: git filters (`filter.<name>.smudge` with a `.gitattributes`
  entry) and other command-valued keys in `.git/config` still run on host
  checkouts. The `git` that a host-run Consumer command starts —
  `prune_command`, the prune script, or `gh` — reads the repository's config
  without the overrides. The prompt bodies are read from the working tree; they reach the
  session, not the host shell.
