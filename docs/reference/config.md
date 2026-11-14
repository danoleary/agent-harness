# `config.toml` reference

`.agent-harness/config.toml` holds a project's non-secret settings (ADR-0008).
Never put a credential here; credentials are
[environment variables](env.md). For a complete, working file see
[`example/.agent-harness/config.toml`](https://github.com/danoleary/agent-harness/blob/main/example/.agent-harness/config.toml).

## How it is checked

The harness loads the file before any sandbox launches. A run reads it from
`origin/main` (`git show origin/main:.agent-harness/config.toml`), never from the
working tree: every sandbox can write the working tree, and the host runs
commands this file declares (ADR-0057). Commit and push a change before a run
picks it up. `agent-harness doctor` checks the working-tree copy.

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
| `skip_when_only` | list of repo-relative glob paths | no |
| `cache_isolate` | list of worktree-relative directories | no |

```toml
[[gates]]
name = "test"
command = "go test ./..."
skip_when_only = [".github/**"]
```

`skip_when_only` applies to PR loop fixes only (`pr-doctor` and any
`github_prs` loop): a conflict resolution, or a CI fix. When every path in the
fix's diff against origin/main matches one of the globs, the loop skips that
gate host-side, logs that it did and why, and pushes. CI must still pass
before merge, so only use it where CI runs the same check and a required status
check guards main. The implementation and review stages, and the
security-alerts loop, ignore it and run every gate. With no list, or an empty
diff, nothing is skipped.

A glob is matched segment by segment: `*`, `?` and `[...]` match within one
path segment, and a `**` segment matches any number of segments, none included.
`.github/**` matches every file under `.github/`; `**/*.md` matches markdown
anywhere; `*.md` matches only markdown at the root.

`cache_isolate` names caches inside the worktree that the gate's host run must
not share with the sandbox session. For each gate run (base gate, review gate,
PR loop re-gate), the harness creates the directory host-side if it is missing
and mounts a fresh, empty tmpfs over it in that gate's container alone. The gate
rebuilds the cache in memory, it is discarded when the container exits, and the
session's copy in the worktree is never touched. herd's Storybook gate rebuilds
the Vite dependency cache, which the session also writes, and a rebuild there
could fail every story file:

```toml
[[gates]]
name = "test-storybook"
command = "cd web && pnpm test-storybook"
cache_isolate = ["web/node_modules/.vite"]
```

A path must be relative, stay inside the worktree (no `..` out of it, not the
worktree itself) and carry no comma. A bad path fails config validation. The
tmpfs counts against the gate container's memory, so list cache directories,
not whole dependency trees. With the key absent, nothing changes.

## `[cache]`

An optional persistent toolchain cache, mounted into every sandbox and gate
container. Omit the table if your toolchain has no useful cache.

| Key | Type | Default | Notes |
|---|---|---|---|
| `volume` | Docker volume name | none | `PNPM_STORE_VOLUME` overrides it. |
| `path` | absolute path in the container | — | Required when `volume` is set. |
| `prune_command` | shell command | none | Runs **on the host** between tickets when free disk is still low after pruning merged worktrees, before a Docker prune. `pnpm store prune`, `go clean -modcache`, `dotnet nuget locals all --clear`. |

## `[sandbox]`

Settings for every container the harness starts: each sandboxed session and
each gate and `post_create` run. Omit the table to take the defaults.

| Key | Type | Default | Notes |
|---|---|---|---|
| `shm_size` | size string, e.g. `"2g"`, `"512m"` | `"1g"` | The container's `/dev/shm`, passed as `docker run --shm-size`. Docker's own default is 64 MB, which crashes headless Chrome tabs. A positive whole number with an optional `b`, `k`, `m` or `g` unit; anything else fails the load. |

## `[tracker]`

Which tracker supplies the tickets, and its non-secret names. The API credential
is env-only. Setup guides: [GitHub Issues](../how-to/github-issues.md),
[Linear](../how-to/linear.md), [Jira](../how-to/jira.md).

| Key | Applies to | Needed | Notes |
|---|---|---|---|
| `kind` | all | yes | `"github"`, `"linear"` or `"jira"`. |
| `findings_label_id` | all | no | The label applied to filed Findings. A label **name** on GitHub and Jira; a label **UUID** on Linear. |
| `ready_label` | github, linear | yes | The label that makes a ticket eligible. On GitHub the harness also adds it to Findings that don't need a human (ADR-0033). Checked at load for Linear only. |
| `blocked_label` | all | no | Screens a ticket out until a person removes the label (on Jira, exclude it in `ready_jql`). The loop adds it to a ticket whose committed branch it refused to push because the review was blocked or a Gate was red (ADR-0042). If it is unset, that ticket is left In Progress with a comment and no label. |
| `repo` | github | yes | `"owner/name"`. Checked when the tracker is built. |
| `in_progress_label` | github | yes | Added on claim, removed on release. |
| `assignee` | github | no | A login to assign on claim, besides the label. |
| `team_key` | linear | yes | The Linear team to select from, e.g. `"ENG"`. A PR loop's project Findings are filed in it. |
| `findings_label` | linear | no | The findings label's **name**, which Linear's issue filter takes. Without it, Findings aren't deduplicated against open issues. |
| `project_key` | jira | yes | The Jira project Findings are filed in. |
| `ready_jql` | jira | yes | JQL for the ready queue. |
| `in_progress_jql` | jira | yes | JQL for claimed tickets, which the stale-claim reaper sweeps. |
| `in_progress_transition` | jira | yes | Workflow transition name that claims a ticket. |
| `todo_transition` | jira | yes | Transition name that releases a ticket. |
| `cancel_transition` | jira | yes | Transition name that closes a ticket the review recommends closing. |
| `findings_issue_type` | jira | no | Issue type for filed Findings. Default `"Task"`. |

"Needed" means the tracker can't work without it. `kind` and the needed Linear
and Jira keys are checked at config load; GitHub's `repo` is checked when the
tracker is built. A Jira transition name that is set but misspelled still
surfaces at first use, for example `transition "In Progres" not available on
issue PROJ-1` at claim time.

## `[feedback]`

Opt-in upstreaming of Harness findings to the public harness repo (ADR-0011). See
[send harness findings upstream](../how-to/upstream-findings.md).

| Key | Default | Notes |
|---|---|---|
| `upstream` | `"off"` | `"off"` keeps Harness findings in `.agent-harness/harness-findings/`; `"github"` files them as issues. |
| `repo` | — | `"owner/name"`. Required when `upstream = "github"`. |
| `findings_label` | none | Label applied to upstreamed issues, and the scope dedup searches. |
| `ready_label` | none | Also applied to each upstreamed finding that is agent-ready: not `needs_human`, not a probable duplicate (ADR-0033). Empty keeps every upstreamed finding behind a human. |
| `project` | the ticket Key | The reporting project's name, stamped on each filing. |

## `[sentry]`

Optional. With it, the harness reads on the host the Sentry issues a ticket
body cites and puts them into the implementation and review prompts (ADR-0041).
The token is the `SENTRY_AUTH_TOKEN` [environment variable](env.md). See
[give sessions the Sentry issues a ticket cites](../how-to/sentry.md).

| Key | Default | Notes |
|---|---|---|
| `org` | — | Required. The organization slug, as in `https://<org>.sentry.io`. |
| `projects` | — | Required. The upper-case short-ID prefixes to look for, e.g. `["HERD"]` for `HERD-3P`. A tracker Key such as `BEH-1148` matches only if you list its prefix. |
| `base_url` | `"https://sentry.io"` | The Sentry site. Set it for a self-hosted Sentry. |

```toml
[sentry]
org = "acme"
projects = ["HERD"]
```

## `[[loops]]`

Optional. An entry with a built-in loop's name (`tickets`, `pr-doctor`, `triage`, `security-alerts`) replaces
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
| `backlog` | none: the tickets that are open, unassigned, and carry neither `ready_label` nor `blocked_label` (on GitHub, nor `in_progress_label`); on Linear, those in a backlog or unstarted state of `team_key`. Linear and GitHub only (ADR-0045) |
| `github_prs` | `scope` (`"all"`: every open PR; `"opted_in"`: PRs on `branch_prefix` or with the label; default `"all"`, or `"opted_in"` when `label` is set), `label` (default `PR_LOOP_LABEL`; not allowed with `scope = "all"`), `when` (`"conflicting"`, `"ci_failing"`; default both), `max_attempts` (default `PR_LOOP_MAX_ATTEMPTS`) |
| `github_security_alerts` | `severity` (any of `"critical"`, `"high"`, `"medium"`, `"low"`; default `["critical", "high"]`), `max_attempts` per alert-set fingerprint (default `2`) |

### `steps`

Each step is `{ use = "<step>" }`, plus `prompt = "<path>"` (relative to
`.agent-harness/`, and inside it) for a Step that runs an agent session.

| Step | Source | Must follow | `prompt` | Checks |
|---|---|---|---|---|
| `implement` | `tracker` | (first) | replaces `prompts/implement.md` | a committed handoff on the branch |
| `review` | `tracker` | `implement` | replaces `prompts/review.md` | every Gate green before the Landing |
| `checkout_pr` | `github_prs` | (first) | not accepted | the branch is at the polled head |
| `fix` | `github_prs` | `checkout_pr` | leads the RebaseFix and CIFix prompts | a clean tree with `origin/main` in its history |
| `new_branch` | `github_security_alerts` | (first) | not accepted | the package's branch is cut fresh from `origin/main` |
| `fix_alerts` | `github_security_alerts` | `new_branch` | leads the SecurityFix prompt | a diff touching only dependency files, and a lockfile that resolves no vulnerable version |
| `gates` | `github_prs`, `github_security_alerts` | `fix` on `github_prs`; `fix_alerts` on `github_security_alerts` | not accepted | every Gate green over the fixed tree |
| `push_existing` | `github_prs` | `gates` | not accepted | pushes only a committed, gate-green tree; rewrites the PR's title and description when the resolution renumbered one of the branch's ADRs |
| `open_pr` | `github_security_alerts` | `gates` | not accepted | opens a new PR only for a committed, gate-green tree |
| `watch_ci` | `tracker`, `github_prs`, `github_security_alerts` | `review` on `tracker`; `push_existing` on `github_prs`; `open_pr` on `github_security_alerts` | not accepted | a CI fix must pass every Gate before it is pushed |
| `assess` | `backlog` | (first) | leads the Triage prompt with the project's own bar for "ready" | a verdict file: ready, or not ready with questions for a human |
| `apply` | `backlog` | `assess` | not accepted | a ready ticket gets `ready_label` (on Linear, Backlog → Todo too); one that is not gets a comment with the questions |
| `retro` | `tracker`, `github_prs`, `github_security_alerts` | any step; must be last | replaces `prompts/retro.md` | files the run's Findings |

A `tracker` loop needs `implement` and `review`. A `github_prs` loop needs
`checkout_pr`, `fix`, `gates` and `push_existing`; `when = ["ci_failing"]` also
needs `watch_ci`, because a red check is fixed inside the CI watch. A
`github_security_alerts` loop needs `new_branch`, `fix_alerts`, `gates` and
`open_pr`. A `tracker` or `github_security_alerts`
loop without `watch_ci` stops once the PR is open and leaves red CI to a PR loop
(ADR-0031); the built-in `tickets` and `security-alerts` loops have it. Without
`retro`, nothing files Findings. A `backlog` loop needs `assess`; without
`apply` it records its verdicts and changes no ticket. Each Step may appear once.

## Prompt bodies

Not keys, but loaded and checked with this file:
`.agent-harness/prompts/implement.md`, `review.md` and `retro.md` must all exist
and be non-blank. See [write the prompt bodies](../how-to/prompt-bodies.md).
