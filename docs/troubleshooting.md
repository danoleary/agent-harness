# Troubleshooting

Start with `agent-harness doctor`. It checks the whole setup in one pass and
prints a fix under every line that fails. This page covers what `doctor` cannot
see. To look up a specific message or exit code, see
[error messages](reference/errors.md).

## The CI watch never sees CI

`GH_TOKEN` must be a **classic PAT with `repo` scope**, SSO-authorized for the
org if the org uses SSO. A fine-grained PAT can push and open the PR, but it
cannot read check runs, so the post-PR CI watch can't observe CI. `doctor`
fails on a fine-grained token for this reason. See [token scopes](reference/tokens.md).

## "Invalid API key" from a subscription token

An `sk-ant-oat01-…` token is an OAuth token. It goes in
`CLAUDE_CODE_OAUTH_TOKEN`, not `ANTHROPIC_API_KEY`. Set exactly one of the two:
when both are set, Claude Code uses `ANTHROPIC_API_KEY`. See
[choose a Claude credential](how-to/claude-credential.md).

## The daemon stalls on a Mac

Idle sleep freezes the daemon mid-run. Set `LOOP_KEEP_AWAKE=1` (in the
environment or your `.env`) and `agent-harness start` holds a
`caffeinate -is -w <loop pid>` assertion until the loop exits, however it exits.
`-s` only blocks system sleep on AC power, and closing the lid still sleeps the
Mac unless it is in clamshell mode. Where there is no `caffeinate` (Linux),
`start` warns and launches anyway.

## The loop stopped and did not restart

That is deliberate. There is no supervisor: a crash, a circuit-breaker trip, or a
`LOOP_MAX_*` ceiling stays down until you relaunch, so a person looks before more
tickets are consumed. The exit code is `0` for a deliberate stop and non-zero for
a crash (the exit-code contract in
[DESIGN.md](DESIGN.md#harness-runtime-host-side)).

## Opaque bash errors in the sandbox

The pinned `claude` CLI's Bash tool surfaces errors that look like the agent's
own bug but come from the environment. You will see them in a session
transcript (`.agent-harness/logs/<KEY>/`). There are four kinds:

1. **Mangled multi-arg bash.** It sometimes mis-parses a single Bash call that
   both pipes into `head`/`tail` and uses a command substitution like
   `cd "$(...)"`. It shows up as `head: invalid number of bytes` or
   `cd: too many arguments`.
2. **Bare `Error` on a plain non-zero exit.** When a command exits non-zero
   _by design_, the tool can collapse that into a bare `Error` string with the
   real exit code and stderr stripped. For example, `git merge-base` exits 1 on
   disjoint histories, which is an expected signal, but the agent can't tell
   that from a real failure.
3. **Dropped variable assignment.** A `VAR=value; cmd "$VAR"` assignment and use
   in _one_ Bash call can expand `$VAR` to the empty string. The failure is
   silent (empty output), or shows up as a path with its prefix missing:
   `DP=/abs; ls "$DP/dist"` becomes `ls: cannot access '/dist'`. Chaining with
   `&&` (`VAR=value && cmd "$VAR"`) expands fine; separating with `;` is what
   drops the variable.
4. **Spurious red gate.** This is the worst one, because a green gate looks red.
   Chaining statements onto a **gate** command in one Bash call, such as
   `pnpm run check > /tmp/check.log 2>&1; echo exit=$?; grep -i error /tmp/check.log | head`,
   can pass those statements to the gate as _arguments_. The formatter inside
   `pnpm run check` then receives `echo exit=$? grep …` as file names, fails
   with `Expected at least one target file`, and pnpm reports
   `[ELIFECYCLE] Command failed with exit code 2`. The gate actually passed. Run
   each gate as its **own** Bash call with nothing appended, and re-run a gate
   **alone** before you believe it failed.

Retrying the same command does not help. The bug is in the bundled CLI's bash
wrapper, so the harness can't patch it. Instead, every prompt carries a steer
(`internal/prompt`) that tells the agent to:

- run one command per Bash call, and avoid `cd "$(...)"`;
- re-run a bare-`Error` command with its exit code captured (`; echo exit=$?`);
- avoid shell variables within one call: inline the absolute path, chain with
  `&&` instead of `;`, or use the Grep/Glob tools with literal absolute paths;
- run each gate as its own Bash call with nothing appended.

If a newer `claude` release fixes it, bump the pinned `CLAUDE_VERSION` in
`Dockerfile.base`.
