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
pnpm install
```

Requires Node **24+** and a working local `docker`.

### Phase 1 — `run-tdd` (current)

The first slice is a manual, single-stage CLI: fetch + claim one hand-passed
ticket, run **only** the `/tdd` session in the sandbox, then verify the worktree
+ handoff commit by ground truth and file any dropped findings. No selection, no
review, no PR, no loop (those are later phases).

```bash
# build the sandbox image once
docker build -t herd-agent-harness:latest .

pnpm run-tdd BEH-362             # fetch + claim, run tdd in the sandbox, verify
pnpm run-tdd BEH-362 --verbose   # also stream the raw agent transcript to the console
pnpm run-tdd BEH-362 --dry-run   # print the prompt + docker command — no mutations, no container
```

`--dry-run` still needs every secret in `.env` and makes one read-only Linear call
(to fetch the ticket the prompt is built from); it does not claim the ticket, launch
the container, or write anything.

After a run, verify by hand: the worktree exists at `.claude/worktrees/beh-362`,
`feat/beh-362` has a commit ahead of `origin/main`, the transcript is at
`agent-harness/logs/<run-id>/BEH-362-tdd.jsonl`, and any findings were filed as
Linear issues.

### Later phases (destination)

```bash
tsx src/main.ts             # loop until queue empty / stopped
tsx src/main.ts --once      # do a single ticket and exit
tsx src/main.ts --verbose   # also stream the agent transcript to the console
```

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
