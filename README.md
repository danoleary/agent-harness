# Agent Harness

Autonomously works the BeHerd Linear backlog, one ticket at a time. For each
ticket it runs three Claude Code sessions — `/tdd` → `/review-worktree` →
`/retrospective` — each inside its own Docker sandbox, then opens a PR.

A Go program (stdlib only, no module dependencies). The host does all remote I/O
(Linear, git, GitHub); the sandbox only runs `claude` and herd's `pnpm` build.

> Lives at `herd/agent-harness/`. Full design, invariants, and failure matrix are
> in [`docs/DESIGN.md`](docs/DESIGN.md); decisions in [`docs/adr/`](docs/adr/).

## Prerequisites

- Go **1.26+**
- `git` **2.45+** (host-side) — the pre-push rebase replays with `git cherry-pick --empty=drop`, which auto-drops a now-redundant feature commit (the flag landed in git 2.45, May 2024; BEH-622)
- A working local `docker` daemon
- A checkout of herd (this repo) — bind-mounted into every sandbox

## Setup

```bash
cd agent-harness
cp .env.example .env     # fill in the four required secrets (see below)
make build               # compile the binaries into bin/
```

The sandbox image builds itself on first run. To control when that ~minutes-long
build happens, pre-build it: `make image`.

### Required secrets (`.env`)

| Var | Purpose |
|---|---|
| `LINEAR_API_KEY` | host-only: select, claim, and move tickets; file findings |
| `CLAUDE_CODE_OAUTH_TOKEN` *or* `ANTHROPIC_API_KEY` | the Claude credential passed into the sandbox (set exactly one) |
| `GH_TOKEN` | host-only: `git push`, `gh pr create`, and the post-PR CI watch |
| `HERD_PATH` | absolute path to the herd checkout to bind-mount |

`LINEAR_API_KEY` and `GH_TOKEN` never enter the sandbox (ADR-0002).

> `GH_TOKEN` **must be a classic PAT with `repo` scope**, SSO-authorized for the
> `Herd-Video-Call-Limited` org. A fine-grained PAT can push and open the PR but
> **cannot read check runs**, so the post-PR CI watch can't observe CI (BEH-476).
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

The next **Todo**, **unassigned**, **`agent-ready`-labelled**, **unblocked**
ticket, ordered by priority then board rank. The `agent-ready` label is the human
gate: a person decides *what* runs unattended; the harness decides *how*.

## Stopping the loop

- **`touch agent-harness/STOP`** — graceful: finishes the current ticket, then
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
internal/          config, linear, prompt, sandbox, session, verify, filing, git, …
Dockerfile         the node-based sandbox image (runs claude + herd's pnpm build)
```

## Known gotcha: opaque bash errors in the sandbox (BEH-401, BEH-598, BEH-601)

The pinned `claude` CLI's Bash tool surfaces opaque errors that look like the
agent's own bug but are an environment artifact, in three ways:

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

Retrying verbatim doesn't help in any case. The bug is in the bundled CLI's
bash wrapper, so the harness can't patch it; instead every prompt carries a steer
(`internal/prompt`) telling the agent to run one command per Bash call, avoid
`cd "$(...)"`, re-run a bare-`Error` command capturing the exit code explicitly
(`; echo exit=$?`) to tell an expected non-zero exit from a failure, and avoid
intra-call shell variables (inline the absolute path, `&&`-chain instead of `;`,
or use the Grep/Glob tools with literal absolute paths). Bump the pinned
`CLAUDE_VERSION` (Dockerfile) if a newer release fixes it upstream.
