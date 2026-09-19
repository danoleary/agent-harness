# Agent Harness

Works a backlog autonomously, one ticket at a time. For each ticket it runs three
Claude Code sessions — implementation → review → retrospective — each in its own
Docker sandbox, then opens a PR.

It is **project-agnostic**. A project adopts it by committing an
`.agent-harness/` directory: its gates, its sandbox image, its prompts, its
tracker. Go, .NET, Node — the harness never learns the difference.

A Go program on the host, which owns every remote operation (tracker, git,
GitHub). The sandbox only ever runs `claude`.

> **Adopting it in your project? Read [`docs/CONSUMER.md`](docs/CONSUMER.md).**
> Full design, invariants and failure matrix are in
> [`docs/DESIGN.md`](docs/DESIGN.md); decisions in [`docs/adr/`](docs/adr/).

## Prerequisites

On the host that runs the harness:

- A working local `docker` daemon
- `git` **2.45+** — the pre-push rebase replays with `git cherry-pick --empty=drop`, which auto-drops a now-redundant feature commit (the flag landed in git 2.45, May 2024)
- `gh`
- Go **1.26+** — only if you build from source

## Install

Download the archive for your platform from
[Releases](../../releases) and put the binaries on your `PATH`. No Go toolchain
needed.

With a Go toolchain, either of:

```bash
go install github.com/danoleary/agent-harness/cmd/loop@latest   # and pipeline, watch, …
make build                                                       # from a checkout, into bin/
```

## Point it at a project

The project needs an `.agent-harness/` directory — see
[`docs/CONSUMER.md`](docs/CONSUMER.md) for the config, the prompt bodies, and how
to build a sandbox image `FROM` the published base.

The harness declares no skill of its own. Each of the three prompt bodies names
the skill its stage invokes, and all three are required: a missing or blank body
fails at config load rather than running a stage with nothing to invoke.

Then set the host environment:

```bash
cp .env.example .env     # fill in the required secrets (see below)
```

The sandbox image builds (or pulls) itself on first run. To control when that
~minutes-long build happens, pre-build it: `make image`.

### Required secrets (`.env`)

| Var | Purpose |
|---|---|
| `PROJECT_PATH` | absolute path to the Consumer checkout to bind-mount |
| `CLAUDE_CODE_OAUTH_TOKEN` *or* `ANTHROPIC_API_KEY` | the Claude credential passed into the sandbox (set exactly one) |
| `GH_TOKEN` | host-only: `git push`, `gh pr create`, and the post-PR CI watch |
| `LINEAR_API_KEY` | host-only, Linear tracker: select, claim and move tickets; file findings |

The tracker credential and `GH_TOKEN` never enter the sandbox (ADR-0002), so a
compromised session cannot move your backlog or touch your remote.

> `GH_TOKEN` **must be a classic PAT with `repo` scope** (SSO-authorized for the
> org, if it uses SSO). A fine-grained PAT can push and open the PR but **cannot
> read check runs**, so the post-PR CI watch can't observe CI.
> See `.env.example` for every optional knob (timeouts, models, loop ceilings).

## Run it

**Autonomous daemon** — works the whole queue and walks away. Detaches with
`nohup`, logs to `loop.log`, writes its PID to `loop.pid`:

```bash
./scripts/loop-start.sh
```

**Watch it run** — a read-only live view of the current ticket and stage, tailing
the global event stream (`logs/loop.jsonl`) the daemon and the pipeline write. It
never controls the loop (Ctrl-C quits the viewer, not the daemon), and works
against `make loop` **or** a single-shot `make pipeline`:

```bash
make watch
```

If no daemon/pipeline is running it prints a clear "loop not running" line and
keeps polling, so it picks up once one starts. See ADR-0005.

**One ticket, start to finish** — implementation → review → retrospective, then
exits. Pass a ticket id, or `--next` to auto-select and claim the top of queue:

```bash
make pipeline ARGS="BEH-362"     # a specific ticket
make pipeline ARGS="--next"      # the next eligible ticket
```

**One stage at a time** — the three tools the pipeline chains, runnable by hand
in this order. Each requires the previous one's worktree:

```bash
make implementation ARGS="BEH-362"    # /tdd → leaves a worktree + commit (no push)
make review ARGS="BEH-362"            # cold review, host re-runs gates, green → push + PR
make retrospective ARGS="BEH-362"     # /retrospective over the transcripts, files findings
```

Every command takes the same flags:

- `--verbose` — also stream the raw agent transcript to the console
- `--dry-run` — print the prompt + docker command and exit (still reads `.env`
  and makes one read-only Linear call; claims nothing, launches no container)

The compiled binaries (`bin/implementation`, `bin/pipeline`, …) and `go run
./cmd/<tool>` work the same way as the `make` targets.

### How a ticket is picked

The next **unstarted**, **unassigned**, **ready-labelled**, **unblocked** ticket,
ordered by priority then board rank. The ready label (`tracker.ready_label` in the
Consumer's config) is the human gate: a person decides *what* runs unattended; the
harness decides *how*.

## Stopping the loop

- **`touch .agent-harness/STOP`** — graceful: finishes the current ticket, then
  exits. The way to wind down a detached `loop-start.sh` daemon.
- **Ctrl-C** once — same graceful stop for a foreground run; twice — hard abort
  (kills the container now, keeps the worktree for review).
- **`kill $(cat loop.pid)`** — hard kill of a detached daemon (escape hatch;
  prefer the STOP file).

There is no supervisor and no auto-restart: a crash, circuit-breaker trip, or
`LOOP_MAX_*` ceiling stays down until you relaunch — by design, so a human looks
before more tickets are consumed. The loop's exit code is `0` for a deliberate
stop and non-zero for a crash (see DESIGN.md §Exit-code contract).

## Logs

Logs are keyed by **ticket id**, so one ticket's whole arc lives in one dir:

```
logs/BEH-NNN/
  <session>-<run-id>.jsonl     full agent transcript per session (tdd, review, retrospective)
  <step>-<run-id>.log          raw stdout for review's non-agent steps (install, gate)
  run.jsonl                    structured event stream shared across the ticket's sessions
  findings/<session>/out.json  the findings dropbox
```

The console prints one concise line per harness event.

## Develop

```bash
make check     # fmt-check + vet + guards + test (the pre-push gate)
make test      # go test ./...
```

Harness `scripts/*.sh` must run under macOS **bash 3.2** — no `declare -A`,
namerefs, `${var,,}`, or `mapfile`. `make check` enforces this (BEH-457).

## Layout

```
cmd/
  implementation/  /tdd → worktree + commit
  review/          cold review + host-side gate re-run + push/PR
  retrospective/   /retrospective, files findings, tears down a clean worktree
  pipeline/        chains the three over one ticket, then exits
  loop/            autonomous daemon over the agent-ready queue
internal/          config, tracker adapters, prompt, sandbox, session, verify, filing, git, …
Dockerfile.base    the published sandbox base image — the harness<->sandbox contract
docs/CONSUMER.md   how a project adopts the harness
```

## Known gotcha: opaque bash errors in the sandbox (BEH-401, BEH-598, BEH-601, BEH-645)

The pinned `claude` CLI's Bash tool surfaces opaque errors that look like the
agent's own bug but are an environment artifact, in four ways:

1. **Mangled multi-arg bash (BEH-401).** It intermittently mis-parses a single
   Bash call that both pipes into `head`/`tail` and uses a command substitution
   like `cd "$(...)"`, surfacing as `head: invalid number of bytes` /
   `cd: too many arguments`.
2. **Bare `Error` on a plain non-zero exit (BEH-598).** When a plain command
   exits non-zero _by design_, the tool can collapse that into a bare `Error`
   string with the real exit code and stderr stripped — e.g. `git merge-base`
   exits 1 on disjoint histories (an expected signal), but the agent can't tell
   that from a real break.
3. **Dropped intra-call variable assignment (BEH-601).** A `VAR=value; cmd
   "$VAR"` assignment-then-use within _one_ Bash call can expand `$VAR` to the
   empty string. The failure is silent (empty output) or surfaces as a path with
   the prefix missing — e.g. `DP=/abs; ls "$DP/dist"` becomes `ls: cannot access
   '/dist'`. `&&`-chaining the assignment to its use (`VAR=value && cmd "$VAR"`)
   expands fine; `;`-separating it is what drops the variable. This bites the
   common `DP=...; rg ... "$DP"` pattern for spelunking a transitive dep's
   `.pnpm` types path.
4. **Spurious gate false-red (BEH-645).** _The worst, because it makes a GREEN
   gate look RED._ Chaining trailing statements onto a **gate** command in one
   Bash call — e.g. `pnpm run check > /tmp/check.log 2>&1; echo exit=$?; grep -i
   error /tmp/check.log | head` — can concatenate those statements as _arguments_
   onto the gate's own command (the `oxfmt --check` inside `pnpm run check`
   receives `echo exit=$? grep …` as file args), so the gate fails with
   `Expected at least one target file. All matched files may have been excluded by
   ignore rules.` and pnpm emits `[ELIFECYCLE] Command failed with exit code 2`.
   That is a spurious failure on a gate that actually _passed_ — do not chase it
   as a real lint/format error or use it to block the diff. Run each verification
   gate (`pnpm run check`/`lint`/`build`/`test…`) as its **own** Bash call with
   nothing appended, and if one reds with `ELIFECYCLE`/`Expected at least one
   target file`, re-run it **alone** before believing it.

Retrying verbatim doesn't help in any case. The bug is in the bundled CLI's
bash wrapper, so the harness can't patch it; instead every prompt carries a steer
(`internal/prompt`) telling the agent to run one command per Bash call, avoid
`cd "$(...)"`, re-run a bare-`Error` command capturing the exit code explicitly
(`; echo exit=$?`) to tell an expected non-zero exit from a failure, avoid
intra-call shell variables (inline the absolute path, `&&`-chain instead of `;`,
or use the Grep/Glob tools with literal absolute paths), and run each verification
gate as its own Bash call with nothing appended. Bump the pinned
`CLAUDE_VERSION` (`Dockerfile.base`) if a newer release fixes it upstream.
