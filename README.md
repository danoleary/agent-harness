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

### Phase 1 — `run-tdd` (current)

The first slice is a manual, single-stage CLI: fetch + claim one hand-passed
ticket, run **only** the `/tdd` session in the sandbox, then verify the worktree
+ handoff commit by ground truth and file any dropped findings. No selection, no
review, no PR, no loop (those are later phases).

```bash
# build the sandbox image once
docker build -t herd-agent-harness:latest .

go run ./cmd/run-tdd BEH-362             # fetch + claim, run tdd in the sandbox, verify
go run ./cmd/run-tdd BEH-362 --verbose   # also stream the raw agent transcript to the console
go run ./cmd/run-tdd BEH-362 --dry-run   # print the prompt + docker command — no mutations, no container
```

Or via the Makefile: `make run-tdd ARGS="BEH-362 --verbose"`. A compiled binary
(`make build` → `bin/run-tdd`) works the same way: `bin/run-tdd BEH-362`.

`--dry-run` still needs every secret in `.env` and makes one read-only Linear call
(to fetch the ticket the prompt is built from); it does not claim the ticket, launch
the container, or write anything.

After a run, verify by hand: the worktree exists at `.claude/worktrees/beh-362`,
`feat/beh-362` has a commit ahead of `origin/main`, the transcript is at
`agent-harness/logs/<run-id>/BEH-362-tdd.jsonl`, and any findings were filed as
Linear issues.

### Later phases (destination)

```bash
go run ./cmd/harness             # loop until queue empty / stopped
go run ./cmd/harness --once      # do a single ticket and exit
go run ./cmd/harness --verbose   # also stream the agent transcript to the console
```

(`cmd/harness` — the selection + review + PR loop — is not built yet; Phase 1 is
`cmd/run-tdd` above.)

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

- Console: one concise line per harness event.
- `logs/<run-id>/BEH-NNN-{tdd,review}.jsonl`: full agent transcripts.
- `logs/<run-id>/run.jsonl`: structured event stream.

## Layout

```
cmd/run-tdd/        Phase-1 CLI entrypoint (the orchestrator)
internal/
  config/           env → resolved HarnessConfig
  ticket/           the Ticket shape
  linear/           host-only Linear GraphQL client + transport (ADR-0001)
  prompt/           builds the sandboxed /tdd prompt
  sandbox/          builds the `docker run …` argv
  stream/           claude stream-json → concise console narration
  findings/         parses the /findings/out.json dropbox
  verify/           ground-truth tdd success check
  git/              gathers worktree + commit ground truth
  runlog/           per-run log dir (console + jsonl + transcripts)
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
