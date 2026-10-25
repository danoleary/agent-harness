# Adopting the harness in your project

A project that uses the harness is a **Consumer**. You adopt it by committing an
`.agent-harness/` directory — never by forking the harness (ADR-0008).

The harness itself runs on your machine, outside your repo. It bind-mounts your
checkout into a container per stage and owns every remote operation host-side.

## What you need

On the **host** (the machine running the harness):

- `docker`, `git` 2.45+, `gh`
- A Claude credential and a tracker credential (see [Secrets](#secrets))

Those credentials live on the host, and you have three places to put them:
`./.env` beside a harness checkout, `.agent-harness/.env` inside your project, or
`${XDG_CONFIG_HOME:-~/.config}/agent-harness/.env` for an install that drives
several projects.

`.agent-harness/.env` is inside the checkout every stage bind-mounts into its
sandbox, so the harness **masks that exact path** in every container — the
session, the gate and `post_create` each mount an empty file over
it. Gitignore it; the mask stops the sandbox reading it, not your git history.
A credential file anywhere *else* in your repo is not masked and is readable by
the agent session.

You do **not** need a Go toolchain. One command installs the harness — no
checkout, no `sudo`:

```bash
curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh
```

It puts a single command, `agent-harness`, on your `PATH` and everything else in
a private `libexec` dir beside it (ADR-0012), along with `.env.example` and this
file under `~/.local/share/agent-harness/`. `agent-harness --help` lists every
command, and `agent-harness doctor` checks your setup, with a fix for each problem it finds. A `go install` or a downloaded archive from [Releases](../../releases)
work too.

## Scaffold it: `agent-harness init`

Run `agent-harness init` in your checkout to write a starting `.agent-harness/`:

```bash
cd ~/src/myproject
agent-harness init            # interactive; --yes accepts every detected default
```

It detects your toolchain from the files at the root: `go.mod`,
`pnpm-lock.yaml`, `*.sln`/`*.csproj`, or `pyproject.toml`/`requirements.txt`.
From that it fills in `post_create`, the gates, `handoff_strip_paths`, a cache
and a Dockerfile `FROM` the pinned base. It takes `tracker.repo` from your GitHub
`origin` remote. `--toolchain` and `--repo` answer those questions up front.
GitHub is the only tracker `init` writes today; for Linear or Jira, edit
`[tracker]` as described below.

It also writes the three prompt bodies, which name **placeholder** skills
(`/TODO-implement-skill`, …). Finally it writes `.agent-harness/.env` from the
credential template, and adds that file to your `.gitignore`.

Every value `init` guesses is marked `TODO(agent-harness init)`. Replace the
skill names and check the gates against your CI before the first run. `init`
never overwrites a file that exists unless you pass `--force`, and it never
overwrites `.env` at all. The defaults it writes for each toolchain are recorded
in ADR-0021.

## The two surfaces you own

### 1. `.agent-harness/config.toml`

Non-secret, project-specific values. Never put a credential here.

```toml
# The sandbox image. Declare EXACTLY ONE of these two shapes:
#
#   dockerfile = "..."   -> the harness BUILDS this on a local image miss, and rebuilds it when the Dockerfile changes
#   image      = "..."   -> a prebuilt, base-compatible ref it PULLS on a miss
#
# Declaring neither is a hard error at config load: there is no sane
# cross-language default, and guessing one would fail opaquely at run time.
image = "myproject-agent-harness:latest"
dockerfile = ".agent-harness/Dockerfile"

# Worktree branch prefix. The harness keys verify/push/PR off `<prefix>/<slug>`.
branch_prefix = "feat"

# Per-worktree toolchain setup, run in a secret-free container against the
# freshly-created worktree (cwd = the worktree, $PROJECT_PATH = the main
# checkout). This is where your deps, generated files and env links go.
#
# It MUST be idempotent. The harness runs it on EVERY provisioning pass,
# including a resumed worktree — a resumed tree routinely has its deps missing
# while still looking ready from the outside. Prefer lockfile-frozen commands
# that no-op when already satisfied.
#
# A failing post_create is only a warning — the session installs what is
# missing. But if the shell cannot run it at all (exit 127 command not found,
# 126 not executable), the run stops before any session: the command is not on
# PATH in this container, and the agent's own container cannot fix that. Make
# sure your image's `ENV PATH` holds every tool post_create and the gates call:
# they run as a non-login `bash -c`, which sources no profile script.
post_create = '''
dotnet restore
'''

# Optional persistent toolchain cache, mounted into every sandbox and gate
# container. Omit the section entirely if your toolchain has no useful cache.
[cache]
volume = "myproject-nuget"
path = "/root/.nuget/packages"
# How to reclaim that cache. The loop daemon runs this between tickets when free
# disk is still below the threshold after pruning merged worktrees — the cheap,
# non-destructive rung before it falls back to a Docker prune. Omitting it skips
# that rung. `pnpm store prune`, `go clean -modcache`, and
# `dotnet nuget locals all --clear` are the usual shapes.
#
# UNLIKE gates and post_create, this runs on the HOST, not in a sandbox — the
# cache it reclaims is the host's. It is shell-interpreted.
prune_command = "dotnet nuget locals all --clear"

# The oldest harness this config is written for. Optional — set it when you start
# relying on a key or behaviour a given release introduced. An older binary
# reading a newer config does not fail on its own: it warns about the keys it
# does not know and otherwise ignores them, so an unknown gate is skipped and a
# renamed field reverts to a default while the run still reports success. The pin
# turns that into an error at startup. Check what you are running with `agent-harness --version`.
min_harness_version = "0.2"

# Which tracker, and its NON-SECRET selection names. The API credential is
# env-only. kind = "linear" | "github" | "jira".
[tracker]
kind = "github"
repo = "myorg/myproject"
findings_label_id = "agent-harness"
ready_label = "ready-for-agent"
in_progress_label = "in-progress"
# blocked_label = "blocked"   # optional; see "Holding a ticket" below

# kind = "linear" instead reads:
#
#   [tracker]
#   kind = "linear"
#   team_key = "PROJ"                  # REQUIRED — the Linear team to select from
#   ready_label = "ready-for-agent"    # REQUIRED — the human-applied gate
#   blocked_label = "Blocked"
#   findings_label_id = "<label-uuid>" # Linear's create API takes the label UUID
#   findings_label = "agent-harness"   # ...and its issue filter takes the NAME
#
# Linear needs the findings label in both spellings because its create API and
# its issue filter disagree about which one they take. `team_key` and
# `ready_label` have no defensible default — an empty team key would query every
# team your credential can see, and an empty ready label would drop the
# blast-radius gate — so both are checked at startup.
#
# Holding a ticket. The harness never picks a ticket a still-open issue blocks,
# so to make a ticket wait on another, link it natively ("blocked by" on GitHub,
# a `blocks` relation on Linear) and leave it ready-labelled: it is picked up on
# the first poll after its last blocker closes, with nobody having to touch it.
# To hold a ticket for a human reason, leave the ready label off. `blocked_label`
# is optional, and redundant beside those two: it names a label that screens a
# ticket out until a human removes it, and nothing ever removes it for you.

# Directory prefixes whose contents can feed a gate or a CI job whatever they look
# like. The harness skips the review gate re-run and the CI poll when a branch's
# whole diff is inert prose OUTSIDE these roots.
#
# OMITTING this disables that short-circuit rather than widening it. That is the
# safe direction: excluding too much only costs a CI poll you did not need, while
# excluding too little means silently merging past a job that could go red. Keep it
# a superset of every root your workflows' `on.pull_request.paths` trigger on.
docs_only_excluded_roots = ["src/", "tests/", ".github/", "scripts/"]

# Worktree-relative paths deleted when the implementation stage hands the worktree
# over. The sandbox builds on linux-arm64, so anything it installed carries
# platform-specific binaries that crash a reviewer's gates on another host; the
# review stage re-provisions the worktree with your `post_create` instead.
#
# Declare only regenerable build output — a pnpm monorepo strips
# `web/node_modules`, a .NET project `obj` and `bin`, a Go project usually
# nothing. Omitting this strips nothing. Paths must stay inside the worktree; an
# absolute or `../` entry is refused at handoff rather than followed.
handoff_strip_paths = ["obj", "bin"]

# Checkout-relative directories the dispatch advisory greps when a ticket cites
# code symbols in backticks. When none of the cited symbols exist there the
# harness warns that the work may have already merged under a sibling ticket.
#
# OMITTING this disables the advisory rather than guessing a path — the safe
# direction. A missed nudge costs one session; scanning the wrong tree reports
# every cited symbol absent and pushes a perfectly valid ticket toward
# "recommend close".
source_roots = ["src"]

# Ordered, named gates. ALL must pass host-side before the harness pushes and
# opens the PR. Language-agnostic — these are just shell commands.
[[gates]]
name = "test"
command = "dotnet test"

[[gates]]
name = "lint"
command = "dotnet format --verify-no-changes"
```

The harness checks this file before any sandbox launches. A syntax error is
reported at its `config.toml:line:column`. Every validation problem is listed in
one error, so you can fix them all in one pass. A key or table the harness does
not read is a warning, not an error, so a config written for a newer harness
still loads; if it is a typo (`[trackr]`), the warning names it and its line.

### 2. `.agent-harness/prompts/`

Three per-stage prompt **bodies**: `implement.md`, `review.md`, `retro.md`. **All
three are required, and each must name the skill its stage invokes.**

A body says which skill to invoke and states your project's conventions. The
harness wraps each one in a non-overridable **contract envelope** — the findings
dropbox protocol, the tracker-off steer, the branch and handoff contract, the
sanitize rule, and the injected ticket context (ADR-0009). A body cannot delete
the envelope; it only fills in what is project-specific.

The harness declares no skill of its own, for any stage. It cannot: it does not
know which skills your project carries. So a missing or blank body is a hard
error at config load, naming every file you still owe it — rather than a stage
that starts a container, claims your ticket, exits 0 and produces nothing.

The skill a body names must exist in **your** repository. A body is also a Go
template, so `{{.Identifier}}`, `{{.Title}}`, `{{.Slug}}`, `{{.BranchPrefix}}`
and `{{.WorktreePath}}` interpolate:

```markdown
/tdd Work on {{.Identifier}}.

Run `dotnet test` before you commit. Commit on `{{.BranchPrefix}}/{{.Slug}}`.
```

## Your sandbox image

If you set `dockerfile`, write one that `FROM`s a **pinned** tag of the published
base and adds your toolchain:

```dockerfile
FROM ghcr.io/danoleary/agent-harness-base:0.6.2

# Your toolchain. Everything here is yours; the base carries only the
# harness<->sandbox contract (claude CLI, git, gosu, the uid-re-exec entrypoint).
RUN apt-get update \
	&& apt-get install -y --no-install-recommends dotnet-sdk-9.0 \
	&& rm -rf /var/lib/apt/lists/*

# Image-internal paths your toolchain needs WRITABLE at run time, colon-separated.
# The container enters as root and drops to the bind-mounted checkout's owner uid,
# so anything you bake owned by the image's uid 1000 is unwritable when the host
# uid differs. The entrypoint chowns each path in this list when that happens.
#
# Your CACHE path does not go here — the harness passes that itself.
ENV HARNESS_CHOWN_PATHS=/opt/my-baked-tool
```

A ticket that edits this Dockerfile has its `post_create` and gates run in an
image built from the branch's copy (`<image>:branch-<fingerprint>`), so a fix to
the toolchain is gated in the image it fixes. Only the Dockerfile's own bytes are
compared: a branch that changes only a file the Dockerfile `COPY`s still runs in
the current image.

Put your toolchain on `PATH` with `ENV PATH=...` in the Dockerfile. The agent
session, the `post_create` hook and every gate all see that same image `PATH`:
the harness runs `post_create` and gates as a non-login `bash -c <command>`,
so `/etc/profile` and `~/.profile` are **not** sourced. Anything a gate needs
must come from the image's `ENV`, not from a login-shell profile script. A
`PATH` you set only in `/etc/profile.d/*.sh` never reaches a gate: the gate
exits 127, and the run stops as an environment failure. Check with
`docker run --rm -e PROJECT_PATH=/tmp <image> bash -c 'command -v <tool>'`, or run
`scripts/check-sandbox-image.sh <image> <tool>...` from the harness repo.

Pin the immutable semver tag, never `:latest`. A base bump should be a
deliberate, reviewable edit, not a silent floating pull.

## Secrets

Host-only, as environment variables. **Never** in `config.toml`.

| Var | Purpose |
|---|---|
| `PROJECT_PATH` | absolute path to the checkout to bind-mount |
| `CLAUDE_CODE_OAUTH_TOKEN` *or* `ANTHROPIC_API_KEY` | the Claude credential — set exactly one |
| `GH_TOKEN` | the harness's own `git push`, `gh pr create`, and the post-PR CI watch |
| `LINEAR_API_KEY` | Linear tracker only |
| `JIRA_BASE_URL` / `JIRA_EMAIL` / `JIRA_API_TOKEN` | Jira tracker only |

Only the Claude credential ever crosses into a container. The tracker and GitHub
credentials stay host-side (ADR-0002): the sandbox never pushes and never talks
to a tracker, so a compromised session cannot move your backlog or your remote.

`GH_TOKEN` must be a **classic** PAT with `repo` scope. A fine-grained PAT can
push and open a PR but cannot read check runs — no "Checks" permission exists for
them — so the post-PR CI watch silently degrades.

See `.env.example` for every optional knob: timeouts, models, and loop ceilings.

The harness reads the first of these files that exists:

| Order | Path | Who it is for |
|---|---|---|
| 1 | `$HARNESS_ENV_FILE` | driving several projects, one credential set each |
| 2 | `./.env` | running from a harness checkout (local beats global) |
| 3 | `<project>/.agent-harness/.env` | keeping credentials with the project (what `init` writes) |
| 4 | `${XDG_CONFIG_HOME:-~/.config}/agent-harness/.env` | one install, several projects |

Exported variables win over the file, so `LINEAR_API_KEY=… agent-harness loop`
also works. `agent-harness doctor` names the file it read and every candidate it
skipped.

## Loops: `agent-harness loop <name>` and `[[loops]]` (optional)

A **loop** is one daemon: a **Source** it polls for work, and the **Steps** it
runs over each **Work item** the Source yields (ADR-0019). Two loops are built in:

| Name | Source | Steps | Runs with |
|---|---|---|---|
| `tickets` | `tracker`: the ready queue | `implement`, `review`, `retro` | `agent-harness loop` (the default) |
| `pr-doctor` | `github_prs`: open PRs that conflict with `main` or have red CI | `checkout_pr`, `fix`, `gates`, `push_existing`, `watch_ci`, `retro` | `agent-harness loop pr-doctor` |

```bash
agent-harness loop --list                # every loop this project can run
agent-harness loop pr-doctor --dry-run   # its Source, Steps and limits as resolved, and the PRs it would work; changes nothing
agent-harness loop pr-doctor             # run it in the foreground
```

You change a built-in loop, or add your own, with `[[loops]]` in `config.toml`.
An entry with a built-in's name replaces it; any other name adds a loop:

```toml
[[loops]]
name = "pr-doctor"
poll_interval = "10m"            # each limit overrides its LOOP_* env knob, for this loop only
max_consecutive_failures = 3
source = { kind = "github_prs", label = "agent-maintained", when = ["conflicting", "ci_failing"] }
steps = [
  { use = "checkout_pr" },
  { use = "fix", prompt = "prompts/fix-pr.md" },   # a Project body, relative to .agent-harness/
  { use = "gates" },
  { use = "push_existing" },
  { use = "watch_ci" },
  { use = "retro" },
]
```

The Sources and Steps are a fixed set, and each is Go code that checks its own
result on the host before the next Step runs. Config chooses them, orders them,
sets their parameters and gives a Step a prompt body. It cannot add a Step that
runs a shell command, or an agent with nothing checking what it did (ADR-0016).

**Loop keys.** `name` (lowercase letters, digits and dashes) and `source.kind` are
required, plus at least one step. The limits are optional; a limit you leave out
keeps its env knob.

| Key | Overrides |
|---|---|
| `poll_interval`, `cap_backoff`, `claim_ttl`, `max_runtime` (Go durations: `"10m"`, `"1h30m"`) | `LOOP_POLL_INTERVAL_MS`, `LOOP_CAP_BACKOFF_MS`, `LOOP_CLAIM_TTL_MS`, `LOOP_MAX_RUNTIME_MS` |
| `max_consecutive_failures`, `max_items` | `LOOP_MAX_CONSECUTIVE_FAILURES`, `LOOP_MAX_TICKETS` |

**Sources.**

| `kind` | Parameters |
|---|---|
| `tracker` | none: the queue is the one `[tracker]` describes |
| `github_prs` | `label` (overrides `PR_LOOP_LABEL`), `when` (`"conflicting"`, `"ci_failing"`; default both), `max_attempts` (overrides `PR_LOOP_MAX_ATTEMPTS`) |

**Steps**, and the order they must come in:

| Step | Source | Must follow | Prompt body | Checks |
|---|---|---|---|---|
| `implement` | `tracker` | (first) | replaces `prompts/implement.md` | a committed handoff on the branch |
| `review` | `tracker` | `implement` | replaces `prompts/review.md` | every Gate green before the Landing |
| `checkout_pr` | `github_prs` | (first) | none | the branch is at the polled head |
| `fix` | `github_prs` | `checkout_pr` | leads the RebaseFix and CIFix prompts | a clean tree with `origin/main` in its history |
| `gates` | `github_prs` | `fix` | none | every Gate green over the fixed tree |
| `push_existing` | `github_prs` | `gates` | none | pushes only a committed, gate-green tree |
| `watch_ci` | `github_prs` | `push_existing` | none | a CI fix must pass every Gate before it is pushed |
| `retro` | either | any step; must be last | replaces `prompts/retro.md` | files the run's Findings |

A `tracker` loop needs `implement` and `review`. A `github_prs` loop needs
`checkout_pr`, `fix`, `gates` and `push_existing`, and `when = ["ci_failing"]`
needs `watch_ci`, because a red check is fixed inside the CI watch. Without
`watch_ci`, a PR loop stops at the push. Without `retro`, nothing files Findings.

The config load checks all of this and reports every problem at once, each at
its `config.toml:line:column`: an unknown source or step, a step out of order, a
prompt file that is missing or blank.

**Stopping and watching a loop.** Each loop has its own sentinel,
`.agent-harness/STOP-<name>`; `touch` it to wind that loop down. The plain
`.agent-harness/STOP` (`STOP_FILE`) winds down every loop. A loop clears both when
it starts. The `tickets` loop logs to `.agent-harness/logs/` as it always has;
every other loop logs to `.agent-harness/logs/loops/<name>/`, one dir per item
plus its own `loop.jsonl`. Follow one with
`agent-harness watch .agent-harness/logs/loops/<name>/loop.jsonl`. Run each loop
in its own daemon, and one daemon per loop per checkout.

## Fixing open PRs: `agent-harness loop pr-doctor` (optional)

A PR can go red on CI, or start conflicting with `main`, after its review stage
has finished. The built-in `pr-doctor` loop is a second daemon that fixes those
PRs. It runs beside the ticket loop (ADR-0019). It replaces the `loop --prs` flag
of earlier releases.

**Which PRs it touches.** A PR qualifies if its head branch is on your
`branch_prefix` (the harness opened it), or if a human added the opt-in label
(`agent-maintained` by default; set the loop's `source.label`, or `PR_LOOP_LABEL`,
to change it). It never works:

- a PR from a fork, because it cannot push there;
- a PR into any base other than `main`;
- a PR labelled `blocked`;
- a PR whose branch is already being worked, either because the PR has the
  `in-progress` label or because the ticket loop holds that branch's ticket In
  Progress;
- a PR failing in a way its `source.when` leaves out.

When your tracker is GitHub Issues, the loop uses your `in_progress_label` and
`blocked_label` names instead of `in-progress` and `blocked`.

**How it resolves a conflict** depends on whose branch it is:

| Branch | Strategy | Push |
|---|---|---|
| On your `branch_prefix` (the harness's) | rebase onto `origin/main` | `--force-with-lease` |
| Opted in with the label (a human's) | merge `origin/main` into the branch | plain push, never forced |

A human's branch is never rewritten, because someone else may have it checked
out. For the same reason, the CI watch does not reactively rebase a human's
branch.

**Red CI.** The loop re-runs the failed checks once, to rule out a flake. If they
are still red, it runs a CIFix session over the failing logs. The fix must be
committed and every Gate must pass before anything is pushed. Pushes to a PR only
append commits; the loop never opens a new PR.

Every fix is checked host-side before it is pushed: the conflict session must
leave a clean tree with `origin/main` in its history, a CI fix must be committed,
and every Gate must be green. If any check fails, nothing is pushed. After a push,
the loop watches CI until it passes or fails, then runs the retrospective over the
session transcripts. Project findings from a PR run are filed only when your
tracker is GitHub Issues. Harness findings are always routed.

**Attempts.** The loop works each PR once per head commit and failure kind, and
records each attempt in a hidden marker comment on the PR, so a restarted daemon
neither retries a commit nor loses count. Any new push starts a new attempt. After
`source.max_attempts` attempts (else `PR_LOOP_MAX_ATTEMPTS`, default 3), the loop
comments on the PR with what it tried and labels it `blocked`. Remove the label to
give it a fresh budget.

```bash
agent-harness loop pr-doctor --dry-run   # list the PRs it would work, and how; changes nothing
agent-harness loop pr-doctor             # run it in the foreground
touch "$PROJECT_PATH/.agent-harness/STOP-pr-doctor"   # wind it down
```

It logs to `.agent-harness/logs/loops/pr-doctor/` (one dir per PR, plus its own
`loop.jsonl`) and needs nothing beyond what the ticket loop needs: `gh` and
`GH_TOKEN` on the host. Run one PR loop per checkout.

## Upstreaming harness findings (optional)

The retrospective stage classifies each finding by **audience**. A *Project*
finding is about your codebase or CI and goes to your tracker. A *Harness*
finding is about the harness, sandbox or contract itself, and by default stays
local in `.agent-harness/harness-findings/`. A new finding there whose key or title
closely matches one already in the dir is still written, but carries
`probable_duplicate_of: <slug>` — merge or delete it when you review the dir.

You can opt in to sending those upstream as issues on the public harness repo:

```toml
[feedback]
upstream = "github"
repo = "danoleary/agent-harness"
findings_label = "harness-finding"
project = "myproject"
```

Findings are sanitized before filing, but read ADR-0011 before enabling this on a
proprietary codebase — the destination is a **public** repo.
