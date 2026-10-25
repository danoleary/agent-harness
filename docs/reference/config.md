# `config.toml` reference

`.agent-harness/config.toml` holds a project's non-secret settings (ADR-0008).
Never put a credential here; credentials are
[environment variables](env.md). For a complete, working file see
[`example/.agent-harness/config.toml`](../../example/.agent-harness/config.toml).

## How it is checked

The harness loads the file before any sandbox launches.

- A syntax error is reported at its `config.toml:line:column`.
- Every validation problem is listed in one error,
  `invalid project config <path> (N problem(s)):`, so you can fix them all in
  one pass.
- A key or table the harness does not read is a **warning**, not an error:
  `<path>:<row>:<col>: unknown key <name>, ignored — fix its spelling, or upgrade agent-harness if it is newer than this binary`.
  A config written for a newer harness still loads, so pin
  `min_harness_version` when you rely on a newer key.

## Top-level keys

| Key | Type | Default | Notes |
|---|---|---|---|
| `image` | string | — | The sandbox image ref. With `dockerfile`, the tag the harness builds; alone, a prebuilt, base-compatible ref it pulls on a miss. `HARNESS_IMAGE` overrides it. |
| `dockerfile` | path, relative to the checkout | — | A Dockerfile `FROM` the sandbox base. The harness builds it on a local image miss and rebuilds it when the file changes. Set `image`, `dockerfile`, or both; setting neither is an error. |
| `branch_prefix` | string | `"feat"` | The harness keys verify, push and PR off `<branch_prefix>/<slug>`. |
| `post_create` | shell command | none | Runs in a secret-free container in each fresh or resumed worktree (cwd = the worktree, `$PROJECT_PATH` = the main checkout). Must be idempotent. A failure is a warning, except exit 126 or 127, which stops the run as an environment failure. |
| `handoff_strip_paths` | list of worktree-relative paths | none | Deleted when the implementation stage hands the worktree over: regenerable build output the linux-arm64 sandbox built (`obj`, `bin`, `web/node_modules`). An absolute or `../` path is refused at handoff. |
| `docs_only_excluded_roots` | list of directory prefixes | none (short-circuit off) | Directories whose files can feed a gate or a CI job. A diff that is only prose outside them skips the host gate re-run and the CI poll. Keep it a superset of every root your CI's `on.pull_request.paths` triggers on. |
| `source_roots` | list of checkout-relative dirs | none (advisory off) | Where the dispatch advisory greps for code symbols a ticket cites. When none exist, it warns the work may have merged under a sibling ticket. |
| `min_harness_version` | string, e.g. `"0.7"` | none | The oldest harness this config is written for. An older binary refuses to start. A local `dev` build skips the check. |
| `pnpm_store_volume` | string | — | Deprecated. Use `[cache]`. |

## `[[gates]]`

Ordered, named shell commands. **At least one is required.** All must pass
host-side, in the sandbox image, before the harness pushes and opens the PR.

| Key | Type | Required |
|---|---|---|
| `name` | string | yes |
| `command` | shell command, run as `bash -c` | yes |

```toml
[[gates]]
name = "test"
command = "go test ./..."
```

## `[cache]`

An optional persistent toolchain cache, mounted into every sandbox and gate
container. Omit the table if your toolchain has no useful cache.

| Key | Type | Default | Notes |
|---|---|---|---|
| `volume` | Docker volume name | none | `PNPM_STORE_VOLUME` overrides it. |
| `path` | absolute path in the container | — | Required when `volume` is set. |
| `prune_command` | shell command | none | Runs **on the host** between tickets when free disk is still low after pruning merged worktrees, before a Docker prune. `pnpm store prune`, `go clean -modcache`, `dotnet nuget locals all --clear`. |

## `[tracker]`

Which tracker supplies the tickets, and its non-secret names. The API credential
is env-only. Setup guides: [GitHub Issues](../how-to/github-issues.md),
[Linear](../how-to/linear.md), [Jira](../how-to/jira.md).

| Key | Applies to | Needed | Notes |
|---|---|---|---|
| `kind` | all | yes | `"github"`, `"linear"` or `"jira"`. |
| `findings_label_id` | all | no | The label applied to filed Findings. A label **name** on GitHub and Jira; a label **UUID** on Linear. |
| `ready_label` | github, linear | yes | The human-applied label that makes a ticket eligible. Checked at load for Linear only. |
| `blocked_label` | github, linear | no | Screens a ticket out until a person removes the label. |
| `repo` | github | yes | `"owner/name"`. Checked when the tracker is built. |
| `in_progress_label` | github | yes | Added on claim, removed on release. |
| `assignee` | github | no | A login to assign on claim, besides the label. |
| `team_key` | linear | yes | The Linear team to select from, e.g. `"ENG"`. |
| `findings_label` | linear | no | The findings label's **name**, which Linear's issue filter takes. Without it, Findings aren't deduplicated against open issues. |
| `project_key` | jira | yes | The Jira project Findings are filed in. |
| `ready_jql` | jira | yes | JQL for the ready queue. |
| `in_progress_jql` | jira | yes | JQL for claimed tickets, which the stale-claim reaper sweeps. |
| `in_progress_transition` | jira | yes | Workflow transition name that claims a ticket. |
| `todo_transition` | jira | yes | Transition name that releases a ticket. |
| `cancel_transition` | jira | yes | Transition name that closes a ticket the review recommends closing. |
| `findings_issue_type` | jira | no | Issue type for filed Findings. Default `"Task"`. |

"Needed" means the tracker can't work without it. Only Linear's needed keys,
and `kind`, are checked at config load; GitHub's `repo` is checked when the
tracker is built. A missing Jira key
surfaces when the harness first uses it, for example
`no transition configured for issue PROJ-1` at claim time.

## `[feedback]`

Opt-in upstreaming of Harness findings to the public harness repo (ADR-0011). See
[send harness findings upstream](../how-to/upstream-findings.md).

| Key | Default | Notes |
|---|---|---|
| `upstream` | `"off"` | `"off"` keeps Harness findings in `.agent-harness/harness-findings/`; `"github"` files them as issues. |
| `repo` | — | `"owner/name"`. Required when `upstream = "github"`. |
| `findings_label` | none | Label applied to upstreamed issues, and the scope dedup searches. |
| `project` | the ticket Key | The reporting project's name, stamped on each filing. |

## `[[loops]]`

Optional. An entry with a built-in loop's name (`tickets`, `pr-doctor`) replaces
it; any other name adds a loop. See
[change a loop or declare your own](../how-to/declare-a-loop.md).

| Key | Type | Required | Default |
|---|---|---|---|
| `name` | lowercase letters, digits, dashes | yes | — |
| `poll_interval` | Go duration (`"10m"`, `"1h30m"`) | no | `LOOP_POLL_INTERVAL_MS` |
| `cap_backoff` | Go duration | no | `LOOP_CAP_BACKOFF_MS` |
| `claim_ttl` | Go duration | no | `LOOP_CLAIM_TTL_MS` |
| `max_runtime` | Go duration | no | `LOOP_MAX_RUNTIME_MS` |
| `max_consecutive_failures` | integer | no | `LOOP_MAX_CONSECUTIVE_FAILURES` |
| `max_items` | integer | no | `LOOP_MAX_TICKETS` |
| `source` | inline table | yes | — |
| `steps` | array of `{ use, prompt }` | yes, at least one | — |

### `source`

| `kind` | Parameters |
|---|---|
| `tracker` | none: the queue is the one `[tracker]` describes |
| `github_prs` | `label` (default `PR_LOOP_LABEL`), `when` (`"conflicting"`, `"ci_failing"`; default both), `max_attempts` (default `PR_LOOP_MAX_ATTEMPTS`) |

### `steps`

Each step is `{ use = "<step>" }`, plus `prompt = "<path>"` (relative to
`.agent-harness/`, and inside it) for a Step that runs an agent session.

| Step | Source | Must follow | `prompt` | Checks |
|---|---|---|---|---|
| `implement` | `tracker` | (first) | replaces `prompts/implement.md` | a committed handoff on the branch |
| `review` | `tracker` | `implement` | replaces `prompts/review.md` | every Gate green before the Landing |
| `checkout_pr` | `github_prs` | (first) | not accepted | the branch is at the polled head |
| `fix` | `github_prs` | `checkout_pr` | leads the RebaseFix and CIFix prompts | a clean tree with `origin/main` in its history |
| `gates` | `github_prs` | `fix` | not accepted | every Gate green over the fixed tree |
| `push_existing` | `github_prs` | `gates` | not accepted | pushes only a committed, gate-green tree |
| `watch_ci` | either | `review` on `tracker`; `push_existing` on `github_prs` | not accepted | a CI fix must pass every Gate before it is pushed |
| `retro` | either | any step; must be last | replaces `prompts/retro.md` | files the run's Findings |

A `tracker` loop needs `implement` and `review`. A `github_prs` loop needs
`checkout_pr`, `fix`, `gates` and `push_existing`; `when = ["ci_failing"]` also
needs `watch_ci`, because a red check is fixed inside the CI watch. A `tracker`
loop without `watch_ci` stops its review stage once the PR is open and leaves red
CI to a PR loop (ADR-0031); the built-in `tickets` loop has it. Without
`retro`, nothing files Findings. Each Step may appear once.

## Prompt bodies

Not keys, but loaded and checked with this file:
`.agent-harness/prompts/implement.md`, `review.md` and `retro.md` must all exist
and be non-blank. See [write the prompt bodies](../how-to/prompt-bodies.md).
