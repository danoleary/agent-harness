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
pnpm install                # or: npm install
tsx src/main.ts             # loop until queue empty / stopped
tsx src/main.ts --once      # do a single ticket and exit
tsx src/main.ts --verbose   # also stream the agent transcript to the console
```

Requires Node **24+** and a working local `docker`.

### Secrets (`.env`)

| Var | Scope | Purpose |
|---|---|---|
| `LINEAR_API_KEY` | host only | ticket selection, claiming, breadcrumbs, filing findings |
| `ANTHROPIC_API_KEY` | passed into sandbox | running `claude` |
| `GH_TOKEN` | sandbox **and** host | sandbox: review's `git push`; host: harness's `gh pr create` |
| `HERD_PATH` | host | path to the herd checkout to mount |

`LINEAR_API_KEY` never enters the sandbox.

## Stopping it

- **Ctrl-C** once: finishes the current ticket, then exits (`will stop after current ticket`).
- **`touch agent-harness/STOP`**: same, for unattended/AFK runs.
- **Ctrl-C twice**: hard abort — kills the running container and exits now,
  leaving the worktree for later review.

## Logs

- Console: one concise line per harness event.
- `logs/<run-id>/BEH-NNN-{tdd,review}.jsonl`: full agent transcripts.
- `logs/<run-id>/run.jsonl`: structured event stream.

See [`docs/DESIGN.md`](docs/DESIGN.md) for the full design, invariants, failure
matrix, and the sandbox image.
