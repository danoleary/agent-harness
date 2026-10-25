# ADR-0021: `agent-harness init` scaffolds a Consumer from recorded toolchain defaults

- Status: Proposed
- Date: 2026-10-25

## Context

A Consumer adopts the harness by committing `.agent-harness/` (ADR-0008). Before
this, the only ways to get one were to write it by hand from CONSUMER.md's long
annotated TOML, or to copy `example/`, which is .NET-only. Most of that
directory follows from two facts about the checkout: which toolchain it builds
with, and which GitHub repo it lives in.

#55 asked for an `init` like `fly launch`. It left two questions for a human:
which per-toolchain defaults to use, and which trackers to support on day one.
This ADR records the answers `init` ships with, so a maintainer can see them
and change them in one place: `internal/scaffold/toolchains.go`.

## Decision

**`agent-harness init` writes a complete, loadable `.agent-harness/` from the
detected toolchain and origin remote. Every guess it makes is marked
`TODO(agent-harness init)` for the Consumer to check.**

- **Toolchain detection.** Markers at the checkout root, checked in this order.
  The first match is the default, and `--toolchain` or the interactive prompt
  overrides it. Defaults are copied from configs the harness already ships or
  documents, so none is new except Python's:

  | Toolchain | Marker | `post_create` | Gates | `handoff_strip_paths` | Cache | Source |
  |---|---|---|---|---|---|---|
  | `go` | `go.mod` | `go mod download` | `go test ./...`, `go vet ./...` | none | `/go` | the harness's own `.agent-harness/` |
  | `pnpm` | `pnpm-lock.yaml`, `pnpm-workspace.yaml` | `pnpm install --frozen-lockfile` | each of `check`, `typecheck`, `lint`, `test` that package.json defines, else `pnpm test` | `node_modules` | `/pnpm-store` | herd, CONSUMER.md |
  | `dotnet` | `*.sln`, `*.slnx`, `*.csproj`, `*.fsproj`, `global.json` | `dotnet restore` | `dotnet test`, `dotnet format --verify-no-changes` | `obj`, `bin` | `/root/.nuget/packages` | `example/` |
  | `python` | `pyproject.toml`, `uv.lock`, `requirements.txt` | `uv sync --frozen` / `uv sync` / `uv pip install -r requirements.txt` | `uv run pytest` | `.venv` | `/uv-cache` | **new**: uv because it manages the venv and the interpreter from one lockfile |

  An npm or yarn project is not detected. Writing pnpm commands for it would
  produce a `post_create` that fails on every worktree.
- **Tracker: GitHub only on day one.** `tracker.repo` comes from the `origin`
  remote, or from `--repo`. GitHub is the one tracker whose required fields can
  be filled from the checkout. Linear's `team_key` and Jira's project cannot be,
  and a placeholder would pass validation and then query the wrong backlog. The
  generated config points at CONSUMER.md for switching.
- **The Dockerfile** `FROM`s the base pin in
  `internal/scaffold/templates/Dockerfile.tmpl`. `scripts/release.sh` bumps that
  pin with the others, so a release's `init` writes that release's base. A test
  holds it equal to the harness's own `.agent-harness/Dockerfile`.
- **Prompt bodies** name `/TODO-implement-skill`, `/TODO-review-skill` and
  `/TODO-retro-skill`. The harness declares no skill of its own (ADR-0009). A
  placeholder that cannot resolve is better than a plausible name that might
  resolve to the wrong skill.
- **The `.env`** is the shipped `.env.example`, with `PROJECT_PATH` set to the
  checkout. It is written to the masked in-repo path with mode 0600, and
  `.agent-harness/.env` plus the run-time artifacts are appended to `.gitignore`.
  It is **never** overwritten, even under `--force`: it is the one file that may
  hold real credentials.
- **No overwrite without `--force`.** Existing files are kept and listed, and
  missing ones are still written. All answers are settled before the first
  write, so a refusal (no toolchain or repo under `--yes`) leaves the checkout
  untouched.
- **The next command is `agent-harness doctor`** (ADR-0020), as #55 asks. It
  checks the scaffolded config, prompt bodies, credentials and tracker in one
  pass and prints a fix under each failure.
- **`init` runs inside the dispatcher.** This extends ADR-0012: `init` is a
  `Builtin` in the dispatch table, not a seventh stage binary — like `doctor`
  (ADR-0020), which also runs in the dispatcher's own process. ADR-0012 exec's
  stage binaries to keep their TTY and signal behaviour. `init` has neither to
  keep, and a new Consumer should be able to scaffold even when the `libexec` dir
  is broken.

## Alternatives considered

- **Leave the per-toolchain defaults for a human, as #55 suggested.** That leaves
  `init` with nothing to write. Copying the defaults from the configs the harness
  already runs, and writing them down here, makes them easy to review and change.
- **Prompt for every key.** That just moves CONSUMER.md into a terminal. `init`
  asks only the two questions that have a detected answer.
- **Support Linear and Jira with placeholder fields.** Rejected for the reason
  above. Adding them later is a new `[tracker]` branch in the config template.

## Consequences

- Changing `.env.example` means copying it to
  `internal/scaffold/templates/env.example`. A test fails until you do.
- A new toolchain is one entry in `toolchains()` plus a row in the table above.
- The Python defaults have never run under the harness. Treat them as the least
  settled row in the table.
