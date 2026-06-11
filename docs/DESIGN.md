# Agent Harness — Design

> Docs for the agent harness are deliberately **separate** from the herd docs
> (`/CONTEXT.md`, `/docs/adr/`). The harness lives at `herd/agent-harness/` for
> now and may move out to its own repo later; nothing here should bleed into
> herd's domain docs.

## What it is

A TypeScript harness that autonomously works the BeHerd Linear backlog one ticket
at a time. For each eligible ticket it runs two Claude Code skill sessions, each
inside its own ephemeral Docker sandbox (so Claude can run with
`--dangerously-skip-permissions` safely):

1. **`/tdd`** — red-green-refactor implementation; ends at a handoff commit on a
   feature branch, no PR.
2. **`/review-worktree`** — a *cold* review of that worktree; runs gates, reviews
   the diff, applies fixes, then **commits and pushes** — but does **not** raise
   the PR.
3. **PR author (Haiku)** — a cheap, short session that reads the ticket + final
   diff + commit messages and emits a PR `{title, body}` as JSON. The **harness**
   then creates the PR host-side with `gh pr create`.

It loops until there are no unblocked, `agent-ready` Todo tickets left, or until
it's told to stop.

PR authoring is split out to its own session (and onto Haiku) because writing a
title/body is cheap summarization, not reasoning — keeping it off the review
model's context and budget. Review still owns the *judgement* (what to fix, what
to push); the author session only *describes* the finished branch.

## Build order

This is built incrementally; the full loop above is the destination, not the
first deliverable. **Phase 1 (the tracer bullet) is a manual, single-stage CLI:**
`run-tdd BEH-NNN` — fetch + claim the ticket, run *only* the `/tdd` session in the
sandbox, show the logs, and prove three things by hand afterward: the worktree was
created, a handoff commit landed, and any harness-improvement findings the session
emitted were filed as Linear issues. No selection, no review, no PR, no loop.
Everything else (review, PR-author, the loop, stop control, automatic selection)
is layered on only once Phase 1 works correctly end-to-end.

## The loop

```
startup:
  fetch + fast-forward origin/main on the primary checkout
  reset consecutive-failure counter

loop:
  if stop requested (Ctrl-C or STOP file)   -> exit cleanly
  ticket = selectNextTicket()               // Linear GraphQL, harness-owned
  if no ticket                              -> exit cleanly ("queue empty")

  Linear: move ticket -> In Progress         // harness owns ALL Linear I/O

  --- session 1: tdd (sandbox, 30 min cap) ---
  run: claude -p "/tdd Work on BEH-NNN. Create the worktree with slug `beh-nnn`. <injected ticket context + findings-dropbox instructions>"
  collectFindings(/findings/out.json) -> harness files any emitted harness-improvement issues   // after EVERY session
  tdd OK  <=> worktree exists AND feat/beh-nnn has >=1 commit ahead of merge-base(origin/main)
  if not OK -> log + Linear breadcrumb comment + skip review + record failure + continue

  fetch + fast-forward origin/main           // "pull main after every session"

  --- session 2: review (sandbox, 15 min cap) ---
  run: claude -p "/review-worktree <worktree-path>  <injected ticket context + 'do not touch Linear; do NOT run gh pr create — commit+push only; append harness-improvement findings to /findings/out.json'>"
  collectFindings(/findings/out.json) -> harness files any emitted harness-improvement issues
  review OK <=> feat/beh-nnn is pushed and ahead of origin/main
  if not OK -> log + Linear breadcrumb comment + KEEP worktree + record failure + continue

  fetch + fast-forward origin/main

  --- session 3: PR author (sandbox, Haiku, 5 min cap) ---
  run: claude -p "/… write a PR title+body from <ticket context + final diff + commit messages>; emit {title, body} JSON" --model claude-haiku-4-5
  author OK <=> valid {title, body} emitted
  if not OK -> harness falls back to a templated title/body (ticket id + commit subjects)

  --- PR creation (harness, host-side) ---
  gh pr create --repo <origin> --head feat/beh-nnn --base main --title <…> --body <…>
  PR OK     <=> gh returned a URL
  if OK     -> git worktree remove <worktree-path>
  if not OK -> log + Linear breadcrumb comment + KEEP worktree + record failure

  fetch + fast-forward origin/main
  reset/advance consecutive-failure counter
  if 3 consecutive ticket failures -> stop + report (circuit breaker)
```

## Key invariants & decisions

- **Sequential only (v1).** One ticket in flight at a time. Worktrees give
  *workspace* isolation (each ticket on its own branch/dir, `main` stays clean),
  but the shared `.git` object/ref store and Linear ticket-selection would race
  under concurrency. Parallelism would need per-ticket *clones*, not worktrees —
  out of scope.
- **Fixed bind-mount path.** The whole herd checkout (incl. `.git` and
  `.claude/worktrees/`) is bind-mounted into every container at one identical
  path (e.g. `/workspace/herd`). A worktree's `.git` pointer stores that
  container path, so **all git ops happen inside containers** at the fixed path;
  the harness on the host only *launches* containers and never runs git inside a
  worktree.
- **Harness picks the ticket; the skill is told which.** Neither skill selects a
  ticket. The harness queries Linear and passes the explicit `BEH-NNN` into the
  `/tdd` prompt.
- **Deterministic slug.** The harness dictates the worktree slug (`beh-nnn`) so
  the path is known without parsing model output: `…/.claude/worktrees/beh-nnn`
  on `feat/beh-nnn`.
- **Success is ground-truth, never self-report.** tdd success = a real commit on
  the branch; review success = a real PR. The agent's own "I'm done" is logged
  but not authoritative.
- **The harness owns all Linear I/O — see [ADR-0001](adr/0001-harness-owns-linear-integration.md).**
  Nothing Linear enters the sandbox. The only secrets in the container are
  `ANTHROPIC_API_KEY` and `GH_TOKEN`.
- **Failure never mutates Linear state and never deletes a worktree.** It logs,
  drops a Linear breadcrumb comment, and continues. Worktrees are the recoverable
  artifact.
- **Pull `origin/main` at startup and after every session.**

## Ticket selection (Linear GraphQL, harness-owned)

A ticket is eligible iff **all** hold:

- workflow state is **Todo** (the team's Todo-type state),
- **unassigned** (never steal human-claimed work),
- carries the **`agent-ready`** label (the human-in-the-loop blast-radius gate —
  a person decides *what* runs unattended; the harness decides *how*),
- **not blocked**: no still-open issue has a `blocks` relation pointing at it, and
  it has no `Blocked` label.

Ordering: **priority** (Urgent → High → Medium → Low → No-priority), tie-broken by
**board sort order** then `createdAt` ascending. Take the top one.

## Harness-improvement findings (both sessions)

Both skills surface findings about the *harness/environment itself* (not the
feature): `tdd` via its session-retrospective step, `review-worktree` via its
systemic-findings step. Under Design B the sandbox has no Linear access, so the
sessions can't file these — the **harness files them after every session**.

Mechanism — a **findings dropbox**, kept deliberately simple and out-of-band:

- A dedicated writable dir is mounted into every session at a fixed container
  path (`/findings`; host `logs/<run-id>/findings/<ticket>-<session>/`), **outside
  the repo tree** so it never dirties the worktree or risks being committed.
- Each session's prompt instructs it: "if you hit problems with the harness itself
  (setup friction, systemic gaps, missing patterns), append them to
  `/findings/out.json` as `{title, body, kind}`; do **not** file Linear issues."
- After **every** session returns (tdd, review — PR-author too, though it rarely
  has any), the harness reads `out.json`, files one Linear issue per finding (team
  BeHerd, referencing the worked ticket), logs each, and clears the file.

These are a separate concern from the feature PR and never go in it — same rule
both skills already follow, just relocated from in-session MCP to post-session
harness filing.

## Credentials & sandbox boundary

| Secret | Where | Used by |
|---|---|---|
| `ANTHROPIC_API_KEY` | container env | running `claude` (incl. Haiku PR-author) |
| `GH_TOKEN` (PAT, repo scope) | **container env + host** | container: review's `git push` (`gh auth setup-git`); host: harness's `gh pr create` against `origin` (no worktree git op) |
| `LINEAR_API_KEY` | **host only** | harness's Linear GraphQL calls |

`web/.env.*` + Supabase link config reach the container **for free** via the
worktree symlinks resolving into the mounted main checkout. Linear never enters
the sandbox (Design B).

The Docker boundary limits filesystem/network blast radius, but the sandboxed
agent runs with the real `ANTHROPIC_API_KEY` + `GH_TOKEN`. That is inherent to
"let it run with `--dangerously-skip-permissions`" — the `agent-ready` label is
the human gate that keeps it bounded.

## Sandbox image

- Node 20, `pnpm` (corepack), `git`, `bash`, the `claude` CLI (pinned), `gh`,
  `supabase` CLI (for schema-touching tdd tickets).
- Entrypoint: `gh auth setup-git`, set git committer identity, `corepack enable`.
- **Persistent pnpm content-addressed store** mounted as a Docker volume so the
  per-worktree `pnpm install` (run twice per ticket on fresh worktrees) is a
  near-instant hardlink op instead of a network fetch.
- **Commit identity:** `Herd Agent Harness <agent-harness@beherd.co>`. The
  skills' existing `Co-Authored-By: Claude` trailer stays.

## Stop control

- **Graceful, per-ticket checkpoint.** Stop only ever lands *between* tickets
  (after a full tdd→review cycle), never mid-ticket — stopping between tdd and
  review would strand a half-reviewed worktree.
- **Two signals, one check** (`stopRequested || existsSync(STOP_FILE)` at each
  between-ticket checkpoint):
  - `SIGINT` (Ctrl-C) — flips the flag, logs `will stop after current ticket`,
    leaves the running session alone.
  - sentinel file `agent-harness/STOP` — the one that matters for AFK runs:
    `touch` it from anywhere and the harness winds down after the current ticket.
- **Double Ctrl-C = hard abort** — kills the running container and exits now,
  leaving the worktree behind (harmless; `review-worktree` can pick it up later).
- The active session is a `docker run` child. To keep "graceful" from actually
  killing it, the harness **spawns the container in its own process group
  (detached) and handles `SIGINT` only in the parent**, flipping the flag rather
  than forwarding the signal.

## Timeouts & failure handling

- Per-session wall-clock caps: **tdd 30 min, review 15 min, PR-author 5 min**
  (configurable). On expiry the harness kills the container and treats the
  session as failed.
- **Failure matrix:**

  | Outcome | tdd | review | PR author + creation |
  |---|---|---|---|
  | Clean success (ground-truth passes) | → run review | branch pushed → run PR author | PR created → remove worktree, ticket done, next |
  | Crash / non-zero exit / timeout | log + breadcrumb, skip rest, next | log + breadcrumb, keep worktree (no push), next | author fails → harness uses a **templated** title/body and still creates the PR |
  | Ran but ground-truth fails | no commit → skip + breadcrumb | not pushed → breadcrumb, keep worktree, next | `gh pr create` returns no URL → breadcrumb, keep worktree, next |

- **Circuit breaker:** 3 consecutive ticket failures → stop and report (assume
  something environmental broke, e.g. expired auth or a broken base build —
  rather than burn the whole `agent-ready` queue failing identically).

## Logging

- **Console = concise harness narration**, one timestamped line per event
  (`selected BEH-312 (Urgent)`, `tdd ✓ committed a1b2c3d`, `review ✓ PR #418`,
  `stop requested — finishing current ticket`, `queue empty — exiting`).
- **Disk = full forensic detail**, per run id:
  - `agent-harness/logs/<run-id>/BEH-NNN-tdd.jsonl` and `…-review.jsonl` — the
    complete `claude … --output-format stream-json` transcript, teed from each
    session.
  - `agent-harness/logs/<run-id>/run.jsonl` — the structured event stream
    (machine-readable mirror of the console).
- `--verbose` tees the raw agent stream to the console too; off by default.

## Harness runtime (host side)

- **Node 24+**, run via `tsx`, no build step, near-zero dependencies: native
  `fetch` (Linear GraphQL), `node:child_process` (driving `docker`), `node:fs`
  (logs + stop file). Tiny dep tree on purpose — this process holds real
  credentials.
- Config: `agent-harness/.env` (`LINEAR_API_KEY`, `ANTHROPIC_API_KEY`,
  `GH_TOKEN`, `HERD_PATH`, bot identity, timeouts) + CLI flags (`--verbose`,
  `--once` for a single ticket then exit, label/timeout overrides).

## Worktree lifecycle

- Created by the `tdd` skill inside the sandbox (`scripts/new-worktree.sh beh-nnn feat`).
- Persists on the host between sessions via the bind mount.
- **On PR-creation success → `git worktree remove`** (branch is pushed and the PR
  captures everything; the worktree is pure disk cost). Run inside a throwaway
  container at the fixed mount path, since the worktree's `.git` pointer is
  container-relative.
- **On any failure → keep it** (recoverable artifact).
