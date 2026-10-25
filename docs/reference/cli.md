# CLI reference

One command, `agent-harness`, is on your `PATH`. `init` and `doctor` run inside
it; every other command replaces it with a stage binary from the private
`libexec` dir (ADR-0012), so that binary's exit code and signal handling pass
straight through.

```
agent-harness <command> [args…]
```

| Command | Arguments | What it does |
|---|---|---|
| [`init`](#init) | `[--yes] [--force] [--toolchain T] [--repo owner/name]` | scaffold this project's `.agent-harness/` |
| [`doctor`](#doctor) | `[--offline]` | check the whole setup; print a fix for every problem |
| [`pipeline`](#pipeline) | `<KEY> \| --next` | one ticket, start to finish, then exit |
| [`start`](#start) | `[<name>] [--instance <id>]` | start a loop as a detached daemon |
| [`loop`](#loop) | `[<name>] [--instance <id>] [--list] [--dry-run]` | run a loop in the foreground until stopped |
| [`watch`](#watch) | `[LOGFILE \| --loop <name> \| --instance <id> \| --all]` | live, read-only view of a running loop |
| [`implementation`](#implementation-review-retrospective) | `<KEY>` | stage 1 alone: implement into a worktree and commit |
| [`review`](#implementation-review-retrospective) | `<KEY>` | stage 2 alone: cold review, host gates, push and PR |
| [`retrospective`](#implementation-review-retrospective) | `<KEY>` | stage 3 alone: file Findings, tear the worktree down |
| `--version` | | print the harness version |
| `--help` | | list the commands |

An unknown command exits 2. A `<KEY>` is a tracker Key: `#57` on GitHub,
`PROJ-123` on Linear or Jira. Quote a GitHub Key in the shell (`'#57'`), or `#`
starts a comment.

## `init`

Scaffolds `.agent-harness/` in the checkout's git root: `config.toml`, a
`Dockerfile` `FROM` the pinned sandbox base, the three prompt bodies, and a
gitignored `.env`. Every guessed value is marked `TODO(agent-harness init)`.

| Flag | Meaning |
|---|---|
| `--yes`, `-y` | accept every detected default without asking |
| `--force` | overwrite files that already exist (never the `.env`) |
| `--toolchain T` | `go`, `pnpm`, `dotnet` or `python` (default: detected from `go.mod`, `pnpm-lock.yaml`, `*.sln`/`*.csproj`, `pyproject.toml`/`requirements.txt`) |
| `--repo owner/name` | the GitHub repo whose issues are the tickets (default: the `origin` remote) |

Exit codes: `0` done, `1` scaffolding failed (no toolchain detected, say), `2` a
bad flag or argument. The per-toolchain defaults are recorded in ADR-0021.

## `doctor`

Checks the install, the host tools, the `.env` it read, every credential (with a
live, read-only API call), the config and prompt bodies, the tracker's
repo/team/project and labels, and the sandbox image. It prints a fix under every
failing line (ADR-0020).

| Flag | Meaning |
|---|---|
| `--offline` | skip every network call; the lines that need one say so |

Exit codes: `0` every check passed, `1` any check failed, `2` a bad argument.

## `pipeline`

```
agent-harness pipeline (<KEY> | --next) [--dry-run] [--verbose] [--force]
```

Runs implementation, then review if implementation succeeded, then the
retrospective always, over one ticket, and exits.

| Flag | Meaning |
|---|---|
| `--next` | select the top eligible ticket and claim it. An empty queue exits 0. |
| `--dry-run` | print the plan and exit: no claim, no container, no tracker change. With `--next`, it selects without claiming. |
| `--verbose` | stream the agent transcript |
| `--force` | start a ticket even though its work looks already merged on `main` |

Exit codes: `0` every stage that ran succeeded, `1` otherwise, including a bad
argument or config.

## `start`

```
agent-harness start [<name>] [--instance <id>]
```

Launches a loop as a detached daemon with `nohup`, writes its pidfile, and
prints how to stop and watch it. With no name it launches the ticket loop
(`tickets`). See [run several instances](../how-to/run-several-instances.md).

| Daemon | Pidfile | Log |
|---|---|---|
| default (`tickets`) | `.agent-harness/loop.pid` | `.agent-harness/loop.log` |
| `<name>` | `.agent-harness/loop-<name>.pid` | `.agent-harness/loop-<name>.log` |
| `--instance <id>` | `.agent-harness/loop-<id>.pid` | `.agent-harness/loop-<id>.log` |

Exit codes: `0` launched, `1` that loop or instance is already running (a stale pidfile
is removed and ignored) or the `loop` binary is missing, `2` a bad argument.
There is no `stop` command: `touch` a STOP sentinel, or `kill` the pid.

## `loop`

```
agent-harness loop [<name>] [--instance <id>] [--list] [--dry-run]
```

Runs one loop in the foreground until it is stopped. With no name it runs
`tickets`, the ready queue; `pr-doctor` fixes open PRs; a `[[loops]]` entry adds
others.

| Flag | Meaning |
|---|---|
| `<name>` | the loop to run (default `tickets`) |
| `--instance <id>` | run as one of several ticket-loop daemons, with its own `STOP-<id>`, pidfile and logs |
| `--list` | print every loop this project can run, and exit |
| `--dry-run` | print the loop's resolved Source, Steps and limits (and, for a PR loop, the PRs it would work), and exit |

**Stopping.** `touch` one of these, or send one SIGINT, and the loop finishes the
current item, then exits:

| Sentinel | Stops |
|---|---|
| `.agent-harness/STOP-<name>` | that loop |
| `.agent-harness/STOP-<id>` | that instance |
| `.agent-harness/STOP` (`STOP_FILE`) | every loop |

A second SIGINT or SIGTERM aborts at once and kills the harness's containers.

**Exit codes.** `0` is a deliberate stop: a STOP, a `LOOP_MAX_*` ceiling, or a
circuit-breaker trip. `1` is a config, tracker or credential error at startup.
`2` is a bad argument or an unknown loop name. `130` is a hard abort. `start`
relies on this: anything but `0` is a crash. Nothing restarts the loop for you.

## `watch`

```
agent-harness watch [LOGFILE | --loop <name> | --instance <id> | --all] [--no-animation] [--no-bell]
```

Tails a loop's event stream and shows the current ticket and stage. It never
controls the loop: Ctrl-C quits the viewer only.

| Argument | Follows |
|---|---|
| (none) | `$PROJECT_PATH/.agent-harness/logs/loop.jsonl`, the default instance |
| `LOGFILE` | that stream, e.g. `.agent-harness/logs/loops/pr-doctor/loop.jsonl` |
| `--loop <name>` | loop `<name>`'s stream, `.agent-harness/logs/loops/<name>/loop.jsonl` |
| `--instance <id>` | instance `<id>`'s stream (with `--loop`, an instance of that loop) |
| `--all` | every instance, each line labelled `[default]`, `[b]`, … |
| `--no-animation` | keep the dashboard, freeze the mascot |
| `--no-bell` | don't ring when the loop stops or stalls |

It reports the daemon down when the pidfile `start` wrote for that loop or
instance names no live process, and "winding down" when any sentinel that stops
it is present: its `STOP-<id>`, its loop's `STOP-<name>`, or the plain `STOP`. A
`LOGFILE` is matched to its daemon by where it sits under `logs/`.

When stdout isn't a terminal, or `NO_COLOR` is set, it prints plain lines. Exit
code `2` on a bad argument.

## `implementation`, `review`, `retrospective`

```
agent-harness <stage> <KEY> [--dry-run] [--verbose] [--force]
```

Each runs one Stage over one ticket: the same Stage `pipeline` would run.
`--force` is read only by `implementation`. Exit codes: `0` the Stage did its job,
`1` otherwise.

## Common flags

- `--help` (or `-h`) prints a command's usage without needing a credential
  (every command but `watch`).
- `--version` prints the harness version.
- `--verbose` streams the agent transcript (`pipeline` and the stage commands).
- A ticket's transcripts and event stream are in `.agent-harness/logs/<KEY>/`.
