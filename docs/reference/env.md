# Environment variables reference

Credentials and runtime knobs are environment variables on the host. They never
go in `config.toml`. The annotated template is
[`.env.example`](../../.env.example), which `agent-harness init` copies to
`.agent-harness/.env`.

## Where the harness reads them

Exported variables win over a file. Otherwise the harness reads the first of
these files that exists:

| Order | Path | Who it is for |
|---|---|---|
| 1 | `$HARNESS_ENV_FILE` | driving several projects, one credential set each |
| 2 | `./.env` | running from a harness checkout |
| 3 | `./.agent-harness/.env` | running from inside the project (what `init` writes) |
| 4 | `$PROJECT_PATH/.agent-harness/.env` | running from elsewhere with `PROJECT_PATH` exported |
| 5 | `${XDG_CONFIG_HOME:-~/.config}/agent-harness/.env` | one install, several projects |

`agent-harness doctor` names the file it read and every candidate it skipped.

`.agent-harness/.env` is inside the checkout every Stage bind-mounts, so the
harness mounts an empty file over that exact path in every container: the
session, each gate and `post_create`. Gitignore it; the mask stops the sandbox
reading it, not your git history. A credential file anywhere *else* in your repo
is not masked, and the agent session can read it.

## Credentials and paths

Only the Claude credential crosses into a container. The tracker and GitHub
credentials stay on the host (ADR-0002). For the scopes each one needs, see
[token scopes](tokens.md).

| Variable | Required | Meaning |
|---|---|---|
| `PROJECT_PATH` | yes | Absolute path to the project checkout to bind-mount. |
| `CLAUDE_CODE_OAUTH_TOKEN` | one of these two | A subscription OAuth token, `sk-ant-oat01-…` ([which to choose](../how-to/claude-credential.md)). |
| `ANTHROPIC_API_KEY` | one of these two | An API key. Also enables the host-side semantic dedup of Findings. Must not be an `sk-ant-oat01-` token. |
| `GH_TOKEN` | yes | Classic PAT, `repo` scope: push, open the PR, watch CI, and the GitHub tracker. |
| `LINEAR_API_KEY` | `kind = "linear"` | Linear API key. |
| `JIRA_BASE_URL` | `kind = "jira"` | Your Jira Cloud site, e.g. `https://acme.atlassian.net`. |
| `JIRA_EMAIL` | `kind = "jira"` | The account the API token belongs to. |
| `JIRA_API_TOKEN` | `kind = "jira"` | An Atlassian API token. |
| `HARNESS_ENV_FILE` | no | An env file to read before every other candidate. |
| `HARNESS_IMAGE` | no | Overrides `image` in `config.toml`. |
| `PNPM_STORE_VOLUME` | no | Overrides `[cache].volume`. |

## Sessions and models

A bad value here falls back to the default.

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_MODEL` | `claude-opus-5-5` | `claude --model` for every Stage's sessions. |
| `TDD_MODEL` | `HARNESS_MODEL` | Model for the implementation session. |
| `REVIEW_MODEL` | `HARNESS_MODEL` | Model for the review session and its CI-fix and rebase-fix sessions. |
| `RETROSPECTIVE_MODEL` | `HARNESS_MODEL` | Model for the retrospective session. |
| `DEDUP_MODEL` | `claude-haiku-4-5-20251001` | Host-side model that checks whether a new Finding repeats a filed one. Needs `ANTHROPIC_API_KEY` on the host. |
| `TDD_TIMEOUT_MS` | `1800000` (30m) | Active-time cap for the implementation session. |
| `TDD_LARGE_REFACTOR_TIMEOUT_MS` | `3600000` (60m) | The cap for a ticket that is a multi-file extract-and-rewire refactor. |
| `REVIEW_TIMEOUT_MS` | `1500000` (25m) | Cap for each review-family session and the host gate run. |
| `RETROSPECTIVE_TIMEOUT_MS` | `2700000` (45m) | Cap for the retrospective session. |
| `SESSION_IDLE_TIMEOUT_MS` | `1200000` (20m) | Kills a session whose output is silent this long. Must stay below every session cap. |

## The post-PR CI watch

A bad value here falls back to the default.

| Variable | Default | Meaning |
|---|---|---|
| `CI_MAX_FIX_ATTEMPTS` | `2` | Diagnose, fix and push cycles against red CI before giving up. |
| `CI_FIX_BUDGET_MS` | `1800000` (30m) | Wall-clock cap on the whole watch-and-fix loop. |
| `CI_POLL_INTERVAL_MS` | `15000` | How often pending checks are re-polled. |
| `CI_POLL_BUDGET_MS` | `720000` (12m) | Cap on one wait for checks to finish. |
| `CI_POLL_STALL_MS` | `480000` (8m) | Give up when the pending check set stops changing this long. |
| `CI_POLL_MAX_BUDGET_MS` | `1500000` (25m) | Keep polling past `CI_POLL_BUDGET_MS`, up to this, while a real gate is still running. |
| `CI_NO_CHECKS_GRACE_MS` | `180000` (3m) | If no check has registered by then and no workflow run exists for the head commit, pass: no workflow applies to the diff. |

## The loop daemon

A bad value here is an error at startup: `<KEY> must be <want>, got "<raw>"`.
A `[[loops]]` entry can override the first six for one loop.

| Variable | Default | Meaning |
|---|---|---|
| `LOOP_POLL_INTERVAL_MS` | `60000` | Wait before re-polling an empty queue. |
| `LOOP_CAP_BACKOFF_MS` | `2700000` (45m) | Backoff after a spending-cap abort, when the abort names no reset time. |
| `LOOP_MAX_CONSECUTIVE_FAILURES` | `3` | Circuit breaker: stop after this many consecutive runs with no PR. |
| `LOOP_MAX_TICKETS` | `0` (unlimited) | Stop cleanly after this many attempted tickets. |
| `LOOP_MAX_RUNTIME_MS` | `0` (unlimited) | Stop cleanly after this much wall-clock time. |
| `LOOP_CLAIM_TTL_MS` | `1800000` (30m) | A claim older than this with no branch or PR is reaped back to Todo. |
| `LOOP_DISK_RECLAIM_THRESHOLD_BYTES` | `8589934592` (8 GiB; `0` off) | Free-disk floor below which the loop prunes merged worktrees between tickets. |
| `STOP_FILE` | `.agent-harness/STOP` | The sentinel that winds every loop down. A relative path resolves against `PROJECT_PATH`. |
| `GATE_CONCURRENCY` | `1` (`0` = no limit) | How many runs may execute their gates at once, across instances sharing the lock dir. |
| `GATE_LOCK_DIR` | `<project>/.agent-harness/locks/` | Where the gate slots live. Share one dir across projects to bound the whole host. |
| `PR_LOOP_LABEL` | `agent-maintained` | The label that opts a human's PR in to `pr-doctor`. |
| `PR_LOOP_MAX_ATTEMPTS` | `3` | Attempts per PR before `pr-doctor` labels it `blocked`. |

## `agent-harness start` and `watch`

| Variable | Read by | Meaning |
|---|---|---|
| `LOOP_KEEP_AWAKE` | `start` | `1`, `true` or `yes`: hold `caffeinate -is -w <loop pid>` on macOS so idle sleep can't freeze the loop. |
| `LOOP_BIN` | `start` | The `loop` binary to launch. The dispatcher sets it. |
| `LOOP_PIDFILE` | `start` | Overrides the pidfile path. |
| `LOOP_LOGFILE` | `start` | Overrides the daemon log path. |
| `WATCH_STALL_AFTER` | `watch` | A Go duration: how long without events before the view shows the loop as stalled. |
| `NO_COLOR` | `watch` | Any value: plain view, no bell. |
| `HARNESS_LIBEXEC` | `agent-harness` | A directory to look in first for the stage binaries. |
