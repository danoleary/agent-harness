# ADR-0020: `agent-harness doctor` checks the whole setup in one pass

- Status: Accepted
- Date: 2026-10-25
- Extends: ADR-0012

## Context

Every Stage checks its setup before it claims a ticket: `config.Load` reports
every environment problem together, `trackers.New` checks the tracker
credential, and `sandbox.Preflight` checks the daemon and the image. Each of
those stops the run at the first layer that fails. A newcomer usually has
several problems at once (no Docker, an old git, a fine-grained `GH_TOKEN`, a
missing prompt body), so they find them one run at a time.

Some problems were never checked before a run:

- A credential that is set but invalid. A revoked key shows up as a failed API
  call partway through a Stage.
- A fine-grained `GH_TOKEN`. It pushes and opens the PR, then cannot read the
  check runs the CI watch depends on.
- A tracker label or team that does not exist. The queue just stays empty.

ADR-0012 made `agent-harness` a dispatcher that `syscall.Exec`s a stage binary
from a private libexec dir.

## Decision

**`agent-harness doctor` runs every check, never stops early, prints a fix under
each failing line, and exits 1 if any check fails.**

The checks are read-only. Each one gives a verdict: ok, warn, fail or skip.

| Check | How |
|---|---|
| install | every stage executable resolves, exactly as a dispatch would |
| docker, git, gh | `docker info` (reaches the daemon); `git --version` ≥ 2.45; `gh --version` |
| `.env` | `config.ResolveEnvFile` and `config.LoadDotEnv`, as a Stage does. Names the file that was read and every candidate that was skipped |
| PROJECT_PATH | `config.ProjectPathProblem`, the same check as `config.Load` |
| Claude credential | `GET /v1/models` with `ANTHROPIC_API_KEY`. Warns when `CLAUDE_CODE_OAUTH_TOKEN` is also set, because Claude Code then uses the API key |
| GH_TOKEN | `GET /user`. `X-OAuth-Scopes` must be present (classic token) and include `repo` |
| config.toml, prompt bodies | `config.LoadProject`, `config.LoadPrompts` |
| tracker | the credential is accepted and the configured repo/team/project and labels exist |
| sandbox image | present (`docker image inspect`), buildable (the declared Dockerfile exists) or pullable (`docker manifest inspect`). Never builds or pulls |

A check that needs an earlier check to pass reports **skip** and names what it
is waiting for. For example, with no usable `PROJECT_PATH` there is no config to
read, so the tracker and image checks skip instead of each failing for the same
reason.

The dispatcher runs `doctor` in its own process. It does not exec a binary for
it. This is the one exception to ADR-0012's "every command is a stage binary":
`doctor` is what an operator runs when the install is broken, so it cannot
depend on the libexec dir it is checking. Signals, TTY and exit code are not an
issue because `doctor` is short-lived and never runs a stage.

Every setup error (a `config.Load` failure from `stages.LoadConfig`, a
`trackers.New` failure, a sandbox preflight refusal) now ends with ``Run
`agent-harness doctor` for a full check.`` (`config.WithDoctorHint`).

The ticket left two questions open. They are answered here:

- **`--offline`: yes.** It skips every network call: the credential and tracker
  checks, and the registry check for a prebuilt image. Those lines report
  *skip* with "not verified (--offline)", so an offline run never counts them as
  passed. A credential that is unset or the wrong shape still fails offline,
  because that needs no network call.
- **`--json`: not now.** The checklist is meant for a person to read, and the
  exit code covers scripts. `Result` is already a plain struct, so adding
  `--json` later only needs a renderer.

## Alternatives considered

- **A seventh stage binary, `cmd/doctor`.** This matches ADR-0012 and keeps the
  dispatcher thin. Rejected: `doctor` would then fail on the same broken
  installs it is meant to diagnose, and the install layout, the release workflow
  and `install.sh` would each need to handle another file.
- **Extending `config.Load` to make the live checks.** Rejected: every Stage
  would pay for the network calls on every run, and `Load` would still stop at
  the first layer that fails.
- **Validating `CLAUDE_CODE_OAUTH_TOKEN` live.** Anthropic has no read-only
  endpoint documented for subscription tokens. A guessed endpoint could reject a
  valid token, and a false failure in `doctor` is worse than no check. So
  `doctor` only checks that the token starts with `sk-ant-oat01-`, and its line
  says that only the shape was checked.

## Consequences

- `doctor` makes live calls to GitHub, Anthropic and the tracker, with a 20s
  timeout for each. On a machine with no network, use `--offline`.
- A fine-grained `GH_TOKEN` is a **fail** in `doctor`, but a Stage still only
  warns about it (`ci.PreflightMessage`): the PR still ships, but its CI is not
  watched. Making it a failure in `doctor` matches the README's "must be a
  classic PAT", and still lets a run started anyway go ahead.
- The tracker checks duplicate a little of what each adapter knows: the Linear
  GraphQL shape, the GitHub label path and the Jira project path. They do not go
  through the `tracker.Tracker` port because the port has no "does this exist?"
  method. Adding one to all three adapters for a diagnostic would make the port
  bigger, not smaller (ADR-0010's lowest-common-denominator rule).
