# Agent Harness

Autonomously works the BeHerd Linear backlog one ticket at a time. For each
eligible ticket it runs three Claude Code sessions — `/tdd`, then
`/review-worktree` (review + fix + push, no PR), then a cheap **Haiku** session
that authors the PR title/body — each in its own Docker sandbox (so Claude runs
with `--dangerously-skip-permissions`). The harness then creates the PR with
`gh pr create`. It loops until the queue is empty or it's told to stop.

> Lives at `herd/agent-harness/` for now; may move to its own repo later. Its docs
> are intentionally separate from the herd docs — see [`docs/DESIGN.md`](docs/DESIGN.md)
> and [`docs/adr/`](docs/adr/).

## How it picks work

The next **Todo**, **unassigned**, **`agent-ready`-labelled**, **unblocked** ticket,
ordered by priority (Urgent → … → No-priority) then board rank. The `agent-ready`
label is the human gate: a person decides *what* runs unattended; the harness
decides *how*.

## Run

```bash
cd agent-harness
cp .env.example .env        # fill in the secrets below
go build ./...              # compile + sanity-check (no external deps)
```

The harness is a Go program (stdlib only — no module dependencies). Requires Go
**1.26+** and a working local `docker`.

### `implementation` — the first of three tools

The build order is three single-role tools, each a thin `cmd/` wrapper over
shared `internal/` plumbing (container launch + transcript tee, ground-truth
verify, findings filing). The first, `implementation`, is a manual, single-stage
CLI: fetch + claim one hand-passed ticket, run **only** the `/tdd` session in the
sandbox, then verify the worktree + handoff commit by ground truth and file any
dropped findings. No push, no PR (review owns those), no loop.

```bash
# build the sandbox image once
docker build -t herd-agent-harness:latest .

go run ./cmd/implementation BEH-362             # fetch + claim, run tdd in the sandbox, verify
go run ./cmd/implementation BEH-362 --verbose   # also stream the raw agent transcript to the console
go run ./cmd/implementation BEH-362 --dry-run   # print the prompt + docker command — no mutations, no container
```

Or via the Makefile: `make implementation ARGS="BEH-362 --verbose"`. A compiled
binary (`make build` → `bin/implementation`) works the same way:
`bin/implementation BEH-362`.

`--dry-run` still needs every secret in `.env` and makes one read-only Linear call
(to fetch the ticket the prompt is built from); it does not claim the ticket, launch
the container, or write anything.

After a run, verify by hand: the worktree exists at `.claude/worktrees/beh-362`,
`feat/beh-362` has a commit ahead of `origin/main`, the transcript is at
`agent-harness/logs/BEH-362/implementation-<run-id>.jsonl`, and any findings were
filed as Linear issues. The log dir is keyed by ticket id, so a second run of the
same ticket adds a new `implementation-<run-id>.jsonl` rather than clobbering the
first.

### `retrospective` — the terminal tool (current)

The third and last tool runs the `/retrospective` skill over a ticket's prior
session transcripts (implementation + review, already visible in-container via
the real-path mount) and files whatever harness-improvement findings the session
drops to Linear. It runs **last** and never claims the ticket — the
implementation tool already moved it to In Progress.

```bash
go run ./cmd/retrospective BEH-362             # run /retrospective over the ticket's transcripts, file findings
go run ./cmd/retrospective BEH-362 --verbose   # also stream the raw agent transcript to the console
go run ./cmd/retrospective BEH-362 --dry-run   # print the prompt + docker command — no container, nothing filed
```

Or via the Makefile: `make retrospective ARGS="BEH-362 --verbose"`, or the
compiled `bin/retrospective BEH-362` (`make build`).

**Ground truth is the presence of the findings dropbox.** After the session, the
harness checks `logs/BEH-362/findings/retrospective/out.json`: an empty `[]` is
success ("ran, found nothing", nothing filed); an **absent** file means the step
never ran and is a failure (the worktree is kept as a breadcrumb). Each finding
in a non-empty array is filed as a Linear issue referencing the worked ticket.
On a fully clean ticket — the branch was pushed (review's host-side gate) *and*
the retrospective filed — the worktree is torn down host-side; if the branch was
never pushed, the worktree is kept so unpushed work is never lost.

### Later phases (destination)

```bash
go run ./cmd/harness             # loop until queue empty / stopped
go run ./cmd/harness --once      # do a single ticket and exit
go run ./cmd/harness --verbose   # also stream the agent transcript to the console
```

(`cmd/harness` — the selection + review + PR loop — is not built yet;
`cmd/implementation` and `cmd/retrospective` above are the runnable tools, with
`cmd/review` — the middle tool — still to land.)

### Secrets (`.env`)

| Var | Scope | Purpose |
|---|---|---|
| `LINEAR_API_KEY` | host only | ticket selection, claiming, breadcrumbs, filing findings |
| `ANTHROPIC_API_KEY` | passed into sandbox | running `claude` |
| `GH_TOKEN` | host only | harness's own `git push` + `gh pr create`; never enters the sandbox |
| `HERD_PATH` | host (also passed into sandbox as the mount path) | path to the herd checkout, bind-mounted at its real path |

`LINEAR_API_KEY` and `GH_TOKEN` never enter the sandbox: the container holds only
the Claude credential and no longer pushes (ADR-0002).

## Stopping it

- **Ctrl-C** once: finishes the current ticket, then exits (`will stop after current ticket`).
- **`touch agent-harness/STOP`**: same, for unattended/AFK runs.
- **Ctrl-C twice**: hard abort — kills the running container and exits now,
  leaving the worktree for later review.

## Logs

Disk logs are **keyed by ticket id**, not run id, so any later tool finds a
ticket's whole arc by globbing one dir:

- Console: one concise line per harness event.
- `logs/BEH-NNN/<session>-<run-id>.jsonl`: full agent transcript per session
  (`implementation-…`, later `review-…`, `retrospective-…`). The run-id suffix
  keeps a re-run from clobbering the first.
- `logs/BEH-NNN/run.jsonl`: structured event stream (shared across the ticket's sessions).
- `logs/BEH-NNN/findings/<session>/out.json`: the findings dropbox.

## Layout

```
cmd/implementation/ the first tool's CLI entrypoint — a thin wrapper
cmd/retrospective/  the terminal tool's CLI entrypoint — runs /retrospective,
                      files findings, tears down a clean worktree (review still to land)
internal/
  config/           env → resolved HarnessConfig (+ shared .env loading)
  ticket/           the Ticket shape
  linear/           host-only Linear GraphQL client + transport (ADR-0001)
  prompt/           builds the sandboxed /tdd + /retrospective prompts
  sandbox/          builds the `docker run …` argv
  session/          launches the container + tees the transcript + narrates
  stream/           claude stream-json → concise console narration
  findings/         parses the /findings/out.json dropbox
  filing/           files parsed findings to Linear + dropbox ground truth (host-side, ADR-0001)
  verify/           ground-truth tdd + retrospective success checks
  git/              gathers worktree + commit ground truth; worktree teardown
  runlog/           ticket-keyed log dir (console + jsonl + transcripts)
Dockerfile,         the *sandbox* image (node-based: runs the claude CLI + herd's
entrypoint.sh         pnpm build) — unrelated to the harness's own language
```

The harness is the **host** program; the `Dockerfile` builds the **sandbox** the
agent runs inside. The sandbox stays node-based because it runs the `claude` CLI
and herd's own `pnpm` build — that's independent of the harness being written in Go.

## Develop

```bash
go test ./...     # full suite
go vet ./...      # static checks
gofmt -w .        # format
make check        # fmt-check + vet + test (the pre-push gate)
```

See [`docs/DESIGN.md`](docs/DESIGN.md) for the full design, invariants, failure
matrix, and the sandbox image.
