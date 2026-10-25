# Agent Harness

Point it at a ticket backlog and it opens tested pull requests, one ticket at a
time, with Claude Code in a Docker sandbox.

For each ticket it runs three Claude Code sessions (implementation → review →
retrospective), each in its own sandbox. It re-runs your gates on the host and
opens a PR only when they are green. It works with any language: your project
declares its gates, sandbox image and prompts in an `.agent-harness/` directory.
The tracker can be GitHub Issues, Linear or Jira.

## Prerequisites

On the machine that runs the harness:

| Tool | Version | Why |
|---|---|---|
| `docker` | a running daemon | every session and gate runs in a container |
| `git` | 2.45+ | the pre-push rebase uses `git cherry-pick --empty=drop` |
| `gh` | any | push, open the PR, watch CI |
| a Claude credential | — | `CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token`) or `ANTHROPIC_API_KEY` |
| `GH_TOKEN` | classic PAT, `repo` scope | a fine-grained token cannot read check runs |
| Go | 1.26+ | only to build from source |

Linear and Jira need their own API credential as well; see
[Linear](docs/how-to/linear.md) or [Jira](docs/how-to/jira.md). For a guided
first run, follow the [tutorial](docs/tutorial.md).

## Quickstart

```bash
curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh
agent-harness init                    # in your project's checkout
agent-harness doctor
agent-harness pipeline --next --dry-run
agent-harness pipeline --next
```

**1. Install.** The installer needs no Go toolchain and no `sudo`. It checks the
release against its published sha256 and puts one command, `agent-harness`, in
`~/.local/bin`. It warns about any missing prerequisite but does not install it.

**2. `init`** scaffolds `.agent-harness/` in your project: a `config.toml`, a
sandbox Dockerfile, the three prompt bodies, and a gitignored `.env` with
`PROJECT_PATH` already filled in. It asks about the toolchain and GitHub repo it
detected (`--yes` takes the defaults) and never overwrites a file without
`--force`. Before going on:

- put your credentials in `.agent-harness/.env`;
- replace each `TODO(agent-harness init)` placeholder, including the skill each
  prompt body invokes.

**3. `doctor`** checks the whole setup in one pass and prints a fix under every
line that fails: the tools, the `.env` it read, each credential (with a live,
read-only API call), the config and prompts, the tracker labels they name, and
the sandbox image. It exits non-zero on any failure. `--offline` skips the
network calls.

**4. `pipeline --next --dry-run`** shows the ticket the harness would pick and
the plan for it, then exits. It claims nothing and starts no container. A ticket
is eligible when it is unstarted, unassigned, unblocked and has your ready label
(`tracker.ready_label`). Applying that label is how a person decides what runs
unattended.

**5. `pipeline --next`** does it for real: it claims the ticket, runs the three
sessions, re-runs the gates, opens the PR, and exits. The first run builds the
sandbox image, which takes a few minutes.

## Next steps

Once a single ticket goes through cleanly, let it work the whole queue:

```bash
agent-harness start      # detached daemon; logs to .agent-harness/loop.log
agent-harness watch      # read-only live view of the current ticket and stage
touch .agent-harness/STOP   # finish the current ticket, then stop
```

`agent-harness pipeline <KEY>` runs one named ticket instead of the next one. In
a foreground run, Ctrl-C once stops after the current ticket; twice kills the
container now and keeps the worktree.

A deep backlog of independent tickets goes faster with a second daemon. Each
instance has its own pidfile, log and `STOP-<id>`; the two never claim the same
ticket, and by default only one runs its gates at a time (`GATE_CONCURRENCY`).
See [run several instances](docs/how-to/run-several-instances.md):

```bash
agent-harness start --instance b    # beside the default instance
agent-harness watch --all           # every instance, each line labeled
touch .agent-harness/STOP-b         # stop instance b alone; plain STOP stops all
```

A second loop, `pr-doctor`, fixes harness PRs (and PRs you label
`agent-maintained`) that went red on CI or conflict with `main` after review.
`--dry-run` prints the loop and what it would do; `--list` names every loop. A
project can change these loops or add its own as `[[loops]]` in `config.toml`. See
[fix open PRs](docs/how-to/fix-open-prs.md) and
[declare a loop](docs/how-to/declare-a-loop.md):

```bash
agent-harness loop pr-doctor --dry-run
```

`agent-harness --help` lists every command. Each one takes `--verbose` (stream
the agent transcript) and `--dry-run`. A ticket's transcripts and event stream
are in `.agent-harness/logs/<KEY>/`.

The [documentation](docs/README.md) is split by what you need:

- **Learn:** [the tutorial](docs/tutorial.md) takes one ticket to a PR, end to end.
- **Do:** how-to guides for [GitHub Issues](docs/how-to/github-issues.md),
  [Linear](docs/how-to/linear.md) and [Jira](docs/how-to/jira.md), the
  [Claude credential](docs/how-to/claude-credential.md),
  [prompt bodies](docs/how-to/prompt-bodies.md), a
  [custom sandbox image](docs/how-to/sandbox-image.md), and
  [troubleshooting](docs/troubleshooting.md).
- **Look up:** [`config.toml` keys](docs/reference/config.md),
  [environment variables](docs/reference/env.md),
  [token scopes](docs/reference/tokens.md), [CLI commands](docs/reference/cli.md)
  and [error messages](docs/reference/errors.md).
- **Understand:** [`docs/DESIGN.md`](docs/DESIGN.md) explains how it works, its
  invariants and its failure handling. Decisions are recorded in
  [`docs/adr/`](https://github.com/danoleary/agent-harness/tree/main/docs/adr).
- [`example/`](https://github.com/danoleary/agent-harness/tree/main/example) is a complete, working `.agent-harness/`.

## More about install

```
~/.local/bin/agent-harness         the command you run
~/.local/libexec/agent-harness/    the stage binaries, kept off your PATH
~/.local/share/agent-harness/      .env.example, README.md, LICENSE, docs/*.md
```

The stage binaries have generic names (`loop`, `watch`, `review`, …), so they stay
out of your `PATH`. `agent-harness <command>` runs them.

<details>
<summary>Other ways to install</summary>

```bash
# A specific release, or a different prefix. Piped into a shell, flags need `sh -s --`:
curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh \
  | sh -s -- --version v0.3.0 --prefix /opt/agent-harness

# Remove it again. Named files only — it never rm -rf's your prefix.
curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh -s -- --uninstall

# From a checkout, into the same layout (Go 1.26+):
make install                      # PREFIX=~/.local by default
make build                        # or just build into ./bin

# With a Go toolchain and no installer. Note this DOES put the generic names in
# your GOBIN — `agent-harness` then dispatches to whichever it finds there.
go install github.com/danoleary/agent-harness/cmd/agent-harness@latest
go install github.com/danoleary/agent-harness/cmd/loop@latest   # and pipeline, watch, …
```

Or download an archive from [Releases](https://github.com/danoleary/agent-harness/releases) and run `./agent-harness`
straight out of the unpacked directory. The archive layout is the installed
layout, so it works in place.

</details>

## Develop

```bash
make check     # fmt-check + vet + guards + test (the pre-push gate)
make test      # go test ./...
make install   # build and install into PREFIX (default ~/.local), like install.sh does
```

`scripts/*.sh` must run under macOS bash 3.2: no `declare -A`, namerefs,
`${var,,}` or `mapfile`. `make check` enforces this.

```
install.sh         the one-command install
cmd/
  agent-harness/   the entrypoint on your PATH: init, doctor, and dispatch to the tools below
  implementation/  /tdd → worktree + commit
  review/          cold review + host-side gate re-run + push/PR
  retrospective/   /retrospective, files findings, tears down a clean worktree
  pipeline/        chains the three over one ticket, then exits
  loop/            autonomous daemon over the ready queue
  watch/           read-only live viewer over the event stream
internal/          config, tracker adapters, prompt, sandbox, session, verify, filing, git, …
scripts/           loop-start.sh (the `start` launcher) + the make check guards
Dockerfile.base    the published sandbox base image: the harness↔sandbox contract
docs/              tutorial, how-to/, reference/, DESIGN.md, adr/ (index: docs/README.md)
```
