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

### `implementation` — the first of three tools (current)

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

### Later phases (destination)

```bash
go run ./cmd/harness             # loop until queue empty / stopped
go run ./cmd/harness --once      # do a single ticket and exit
go run ./cmd/harness --verbose   # also stream the agent transcript to the console
```

(`cmd/harness` — the selection + review + PR loop — is not built yet; the first
tool is `cmd/implementation` above, with `cmd/review` and `cmd/retrospective` to
follow.)

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
cmd/implementation/ the first tool's CLI entrypoint — a thin wrapper (review +
                      retrospective land beside it later)
internal/
  config/           env → resolved HarnessConfig
  ticket/           the Ticket shape
  linear/           host-only Linear GraphQL client + transport (ADR-0001)
  prompt/           builds the sandboxed /tdd prompt
  sandbox/          builds the `docker run …` argv
  session/          launches the container + tees the transcript + narrates
  stream/           claude stream-json → concise console narration
  findings/         parses the /findings/out.json dropbox
  filing/           files parsed findings to Linear (host-side, ADR-0001)
  verify/           ground-truth tdd success check
  git/              gathers worktree + commit ground truth
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
