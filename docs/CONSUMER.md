# Adopting the harness in your project

A project that uses the harness is a **Consumer**. You adopt it by committing an
`.agent-harness/` directory — never by forking the harness (ADR-0008).

The harness itself runs on your machine, outside your repo. It bind-mounts your
checkout into a container per stage and owns every remote operation host-side.

## What you need

On the **host** (the machine running the harness):

- `docker`, `git` 2.45+, `gh`
- A Claude credential and a tracker credential (see [Secrets](#secrets))

Those credentials live on the host, in `./.env` or
`${XDG_CONFIG_HOME:-~/.config}/agent-harness/.env` — **never in the repository
the harness works.** That checkout is bind-mounted into every sandbox, so a
credential file inside it is readable by the agent session.

You do **not** need a Go toolchain. Download a binary from
[Releases](../../releases), or `go install` if you prefer.

## The two surfaces you own

### 1. `.agent-harness/config.toml`

Non-secret, project-specific values. Never put a credential here.

```toml
# The sandbox image. Declare EXACTLY ONE of these two shapes:
#
#   dockerfile = "..."   -> the harness BUILDS this on a local image miss
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
post_create = '''
dotnet restore
'''

# Optional persistent toolchain cache, mounted into every sandbox and gate
# container. Omit the section entirely if your toolchain has no useful cache.
[cache]
volume = "myproject-nuget"
path = "/root/.nuget/packages"

# Which tracker, and its NON-SECRET selection names. The API credential is
# env-only. kind = "linear" | "github" | "jira".
[tracker]
kind = "github"
repo = "myorg/myproject"
findings_label_id = "agent-harness"
ready_label = "ready-for-agent"
blocked_label = "blocked"
in_progress_label = "in-progress"

# Directory prefixes whose contents can feed a gate or a CI job whatever they look
# like. The harness skips the review gate re-run and the CI poll when a branch's
# whole diff is inert prose OUTSIDE these roots.
#
# OMITTING this disables that short-circuit rather than widening it. That is the
# safe direction: excluding too much only costs a CI poll you did not need, while
# excluding too little means silently merging past a job that could go red. Keep it
# a superset of every root your workflows' `on.pull_request.paths` trigger on.
docs_only_excluded_roots = ["src/", "tests/", ".github/", "scripts/"]

# Ordered, named gates. ALL must pass host-side before the harness pushes and
# opens the PR. Language-agnostic — these are just shell commands.
[[gates]]
name = "test"
command = "dotnet test"

[[gates]]
name = "lint"
command = "dotnet format --verify-no-changes"
```

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
FROM ghcr.io/danoleary/agent-harness-base:0.1.0

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

## Upstreaming harness findings (optional)

The retrospective stage classifies each finding by **audience**. A *Project*
finding is about your codebase or CI and goes to your tracker. A *Harness*
finding is about the harness, sandbox or contract itself, and by default stays
local in `.agent-harness/harness-findings/`.

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
