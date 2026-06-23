# Agent Harness — Design

> Docs for the agent harness are deliberately **separate** from the herd docs
> (`/CONTEXT.md`, `/docs/adr/`). The harness lives at `herd/agent-harness/` for
> now and may move out to its own repo later; nothing here should bleed into
> herd's domain docs.

## What it is

A Go harness that works the BeHerd Linear backlog one ticket at a time through
**three single-role tools**, each a separate binary under `cmd/`, each taking a
ticket id, each running one Claude Code skill inside its own ephemeral Docker
sandbox (so Claude can run with `--dangerously-skip-permissions` safely):

1. **`implementation`** (skill: `/tdd`) — red-green-refactor implementation; ends
   at a handoff commit on `feat/beh-nnn`. No push, no PR.
2. **`review`** (skill: `/review-worktree`, steered off its findings/push/PR
   steps) — a *cold* review of the worktree diff; applies fixes as a local commit.
   The **harness** (host-side, after the container exits) then independently
   re-runs the quality gates in a throwaway container and, **only if they pass,
   pushes the branch and opens the PR** with `gh`. It then **watches GitHub CI**
   for the PR and, on a red check, runs a bounded auto-fix loop (diagnose + fix in
   the sandbox over the same worktree, push, re-poll) before reporting the terminal
   CI result — local gates green is not CI green (BEH-414).
3. **`retrospective`** (skill: `/retrospective`, new) — runs **last**; reads
   *every prior session's transcript* for the ticket plus the diff, and writes
   harness/environment findings to `/findings/out.json` (**always**, even `[]`).
   The harness then files each finding to Linear.

**Three agents, three roles. The contract between them is the shared host
checkout** — the worktree (code) and the ticket-keyed transcripts — **plus the
`/findings/out.json` dropbox.** Each tool advances that shared state; none of
them reaches a remote. This single-role split is deliberate: in an earlier
two-role design the findings/retrospective step lived *inside* `/tdd` and was
silently skipped (the agent ended with a summary and never wrote `out.json`), so
"no systemic issues found" was indistinguishable from "the step never ran."
Making retrospective its own agent with a single job — and making an absent
`out.json` a hard failure (see ground truth below) — closes that gap.

The eventual destination is an autonomous loop over the `agent-ready` queue (see
**The loop**), but the three tools are independently runnable by hand first.

## Build order

This is built incrementally; the autonomous loop is the destination, not the
first deliverable. The build order is the three tools, in pipeline order, each
runnable by hand against a ticket id before the next is added:

1. **`implementation BEH-NNN`** (was `run-tdd`) — fetch + claim the ticket, run
   *only* `/tdd` in the sandbox, verify by ground truth that the worktree exists
   and a handoff commit landed. Before claiming or launching, it skips a ticket
   whose work already merged on `main` (`git.TicketAlreadyOnMain` greps recent
   `origin/main` history for the key, fetching first) — re-dispatching it would
   burn a worktree + install to discover an empty diff and wrongly flip a done
   ticket to In Progress (BEH-528). `--force` overrides for a false positive.
2. **`review BEH-NNN`** — run `/review-worktree` over the existing worktree, then
   host-side re-run the gates and, if green, push + open the PR.
3. **`retrospective BEH-NNN`** — run `/retrospective` over the ticket's
   transcripts, then file any `out.json` findings to Linear.

Selection, stop control, the circuit breaker, and the loop that chains the three
are layered on only once the tools work correctly by hand. The three binaries
share their plumbing (container launch, transcript tee, ground-truth verify,
findings filing) via `internal/` so each `cmd/` entry stays thin.

## The pipeline

The **pipeline** is the single-ticket chain that sits between the three
hand-tools and **The loop**: `pipeline BEH-NNN` runs implementation → review →
retrospective over *one* hand-passed ticket, then exits. It has no ticket
selection, no stop control, and no circuit breaker — those belong to the loop,
which is "select a ticket, run the pipeline, repeat." The three standalone tools
stay exactly as they are; the pipeline is an addition, not a replacement.

```
pipeline BEH-NNN:
  fetch + fast-forward origin/main on the primary checkout   // once, at the top — not per stage

  --- implementation ---
  run the implementation stage (claim, /tdd, ground-truth verify)
  if not OK -> record failure; SKIP review (nothing to review)

  --- review (only if implementation OK) ---
  run the review stage (cold /review-worktree, host-side gate + push + PR)
  if not OK -> record failure

  --- retrospective (ALWAYS, even if a prior stage failed) ---
  run the retrospective stage (/retrospective over logs/BEH-NNN/, file findings)
  # its value is highest on a failed slice — that's the run worth mining for findings

  exit 0 iff every stage that RAN succeeded; non-zero otherwise
```

Design decisions, and why:

- **In-process, not subprocesses.** Each tool's `run()` body moves out of its
  `cmd/<tool>/main.go` into `internal/stages`, returning a typed result; both the
  existing `cmd/` wrappers *and* `cmd/pipeline` call it. This completes the
  established "thin `cmd/` over shared `internal/`" shape — the pipeline shares
  one config load and one `runlog`, gets real per-stage results instead of opaque
  exit codes, and stays a single process for clean Ctrl-C / timeout handling.
  Shelling out to `bin/*` would re-parse args, re-load `.env`, couple the pipeline
  to built binaries on `PATH`, and reduce each stage to an exit code.
- **Fast-forward `main` once, at the top — not per stage.** The three stages run
  seconds apart in one uninterrupted run, so `main` won't meaningfully move
  mid-pipeline; one fetch+ff keeps the worktree's merge-base honest for impl's
  "commits ahead of merge-base" check and review's gates. The loop's *per-session*
  ff exists because it runs many tickets over a long span — that rationale does
  not transfer to a single quick pipeline.
- **Retrospective always runs** (see the stop/skip rule above) — this is the one
  place the pipeline deliberately diverges from the loop's `if not OK -> skip
  rest`, because a failed slice is exactly the run whose transcripts are worth
  mining.
- **`--verbose`** forwards to all stages. **`--dry-run`** is pipeline-level: it
  prints the *plan* (ordered stages + each stage's resolved prompt/docker command
  as best it can be computed) and explicitly notes that the review/retro commands
  assume impl's worktree/transcripts, which don't exist under dry-run. It claims
  nothing, launches nothing, touches no Linear. No subset/`--only`/resume flags —
  independence is already served by the three standalone binaries.
- **Exit code is plain `0`/`1`.** Per-stage verdicts are already logged loudly via
  `runlog`; the loop will read the structured stage results, not the exit code, so
  encoding which stage failed into the code buys nothing.

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

  --- implementation: /tdd (sandbox, 30 min cap) ---
  run: claude -p "/tdd Work on BEH-NNN. Create the worktree with slug `beh-nnn`. <injected ticket context>"
  impl OK <=> worktree exists AND feat/beh-nnn has >=1 commit ahead of merge-base(origin/main)
  if not OK -> log + Linear breadcrumb comment + skip rest + record failure + continue

  fetch + fast-forward origin/main           // "pull main after every session"

  --- review prep: repopulate web/node_modules before the cold session (BEH-490) ---
  throwaway install container: `pnpm install --frozen-lockfile` in the worktree
  # handoff strips node_modules (BEH-412); pre-install so the session doesn't pay it mid-gate. warn-only.

  --- review: /review-worktree (sandbox, 15 min cap) ---
  run: claude -p "/review-worktree <worktree-path>  <injected ticket context + 'do not touch Linear; commit locally ONLY — do NOT push, do NOT run gh; do NOT emit findings (retrospective owns that)'>"
  # agent has no GH_TOKEN; it can only commit into the shared local .git

  --- review ground truth + push gate (harness, host-side) ---
  re-run gates in a throwaway container: `pnpm check && pnpm build` on feat/beh-nnn
  review OK <=> gates are GREEN          // never the agent's self-report
  if OK     -> git -C $HERD_PATH push origin feat/beh-nnn
               gh pr create --repo <origin> --head feat/beh-nnn --base main \
                            --title <templated> --body <templated: ticket id + commit subjects>
  if not OK -> log + Linear breadcrumb comment + KEEP worktree + record failure + continue

  --- review CI watch + auto-fix (harness, host-side, BEH-414) ---
  poll `gh pr checks feat/beh-nnn` until terminal (success/failure/cancelled), bounded by a poll budget
  if green        -> done
  else (red):
    one re-run of the failed checks first (flake wash); re-poll
    while still red AND attempts < N AND within wall-clock budget:
      fetch failed-job logs (`gh run view <id> --log-failed`, host-side)
      run sandboxed fix session over the worktree (diagnose + fix + LOCAL commit)
      harness pushes the fix; re-poll
    on green   -> done
    on exhaust -> non-success: KEEP PR + worktree, print failing checks + logs pointer

  fetch + fast-forward origin/main

  --- retrospective: /retrospective (sandbox, 10 min cap) ---
  run: claude -p "/retrospective for BEH-NNN. Read every transcript under logs/BEH-NNN/ + the diff. Append harness/environment findings to /findings/out.json (ALWAYS write the file, even as []). Touch no code, no Linear."
  retro OK <=> /findings/out.json EXISTS          // absent => the step never ran => failure
  collectFindings(/findings/out.json) -> harness files one Linear issue per finding (or none, for [])
  if not OK -> log + Linear breadcrumb comment + KEEP worktree + record failure

  if review OK -> git worktree remove <worktree-path>   // branch pushed + PR captures everything

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
- **Real-path bind mount — see [ADR-0002](adr/0002-harness-owns-remote-io.md).**
  The whole herd checkout (incl. `.git` and `.claude/worktrees/`) is bind-mounted
  into every container at **its own real host path** (`$HERD_PATH:$HERD_PATH`,
  `-w $HERD_PATH`), not a synthetic `/workspace/herd`. A worktree's `.git` pointer
  is an absolute path, so mounting at the real path makes it resolve **identically
  in every container and on the host** — a human can `cd .claude/worktrees/beh-nnn
  && git diff` to inspect live work. (The path is still fixed *per machine*, so
  the cross-container invariant the worktree contract relies on still holds.)
- **Harness picks the ticket; the skill is told which.** Neither skill selects a
  ticket. The harness queries Linear and passes the explicit `BEH-NNN` into the
  `/tdd` prompt.
- **Deterministic slug.** The harness dictates the worktree slug (`beh-nnn`) so
  the path is known without parsing model output: `…/.claude/worktrees/beh-nnn`
  on `feat/beh-nnn`.
- **Success is ground-truth, never self-report.** Per tool: *implementation* = a
  real commit ahead of merge-base; *review* = the harness's **own** re-run of the
  gates is green (which is also the push gate — no branch reaches a PR on the
  agent's say-so); *retrospective* = `/findings/out.json` exists on disk (an empty
  `[]` is a valid "ran, found nothing"; an *absent* file means the step never ran
  and is a failure). The agent's own "I'm done" is logged but never authoritative.
- **A usage-policy refusal is retryable, not fatal ([BEH-389](https://linear.app/beherd/issue/BEH-389)).**
  Claude Code's "unable to respond … violate our Usage Policy" refusal is a known
  intermittent false-positive on long agentic sessions; it returns a terminal
  `is_error` result that would otherwise sink an on-scope run. Because the diff
  survives on disk (the real-path bind mount), *implementation* retries the tdd
  session once on the same ticket when a refusal left no handoff commit, and on a
  final failure captures any uncommitted worktree work as a **harness recovery
  checkpoint commit** ([BEH-479](https://linear.app/beherd/issue/BEH-479)) so a
  finished-but-uncommitted diff (the wall-clock cap firing mid-verification, a
  refusal, a crash) is a `git log` away on `feat/beh-nnn` rather than a bare
  worktree needing manual rescue. The checkpoint does **not** flip the verdict —
  the run still fails and its subject (`checkpoint(harness): …`) loudly marks it
  unverified so a reviewer never mistakes it for a real handoff. To keep refusals rare, the tdd
  session is pinned to an exact Opus snapshot (`claude-opus-4-8`), not the floating
  `opus` alias that once resolved to a stale, refusal-prone Opus 4.1.
- **The harness owns all remote I/O — Linear ([ADR-0001](adr/0001-harness-owns-linear-integration.md))
  *and* git push / PR ([ADR-0002](adr/0002-harness-owns-remote-io.md)).** Agents
  commit only into the local shared `.git`; the harness pushes (via the main
  checkout) and opens the PR host-side. The sandbox is air-gapped except for
  Anthropic: the **only** secret in the container is the Claude credential
  (`ANTHROPIC_API_KEY` *or* `CLAUDE_CODE_OAUTH_TOKEN`). No `GH_TOKEN`, no Linear.
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

## Harness-improvement findings (retrospective owns this, alone)

Findings about the *harness/environment itself* (setup friction, systemic gaps,
missing patterns) — **not** the feature — are produced by exactly one agent:
`retrospective`. `implementation` and `review` no longer emit findings; this is a
deliberate change from the earlier design where both did. A single writer means
no merge races on `out.json`, one clear owner, and the systemic judgement living
in one model's context. Review's in-the-moment "this fought me" instinct survives
into its transcript, which retrospective reads.

**Input — the prior transcripts are a first-class input.** Retrospective reviews
the *sessions*, not just the code, so it needs what happened during them. With the
real-path mount, the ticket-keyed transcripts at
`$HERD_PATH/agent-harness/logs/BEH-NNN/` are already visible inside the container
at their natural path — no extra mount. Retrospective reads **every** prior
transcript for the ticket (implementation *and* review) plus the diff.

**Output — the findings dropbox**, kept deliberately simple and out-of-band:

- A dedicated writable dir is mounted at a fixed container path (`/findings`;
  host `logs/BEH-NNN/findings/retrospective/`), **outside the repo tree** so it
  never dirties the worktree or risks being committed.
- The retrospective prompt instructs it: append findings to `/findings/out.json`
  as a JSON array of `{title, body, kind}`; do **not** file Linear issues; and
  **always write the file, even as `[]`** (so absence means the step never ran).
- After the session returns, the harness reads `out.json`, files one Linear issue
  per finding (team BeHerd, referencing the worked ticket), and logs each.

These are a separate concern from the feature PR and never go in it.

> Why `review` is a **cold** review and does *not* read the implementation
> transcript: coldness is the point — review reconstructs intent from the
> branch/issue/diff so it isn't anchored to the implementer's framing. Only
> retrospective, whose job *is* to study the sessions, reads the transcripts.

## Credentials & sandbox boundary

| Secret | Where | Used by |
|---|---|---|
| `ANTHROPIC_API_KEY` *or* `CLAUDE_CODE_OAUTH_TOKEN` | **container env** | running `claude` (the only secret that crosses the boundary) |
| `GH_TOKEN` (PAT, repo scope) | **host only** | harness's `git push` (via the main checkout) + `gh pr create` against `origin` |
| `LINEAR_API_KEY` | **host only** | harness's Linear GraphQL calls |

`web/.env.*` + Supabase link config reach the container **for free** via the
worktree symlinks resolving into the mounted main checkout. Neither Linear nor
GitHub credentials ever enter the sandbox (ADR-0001, ADR-0002).

The Docker boundary limits filesystem/network blast radius, and — unlike the
original design — the sandboxed agent now holds **only the Claude credential**, so
even a fully compromised `--dangerously-skip-permissions` session cannot push to a
remote, open a PR, or touch Linear; the worst it can do is mutate the local
checkout, which the harness's independent gate re-run catches before anything
ships. The `agent-ready` label remains the human gate on *what* runs unattended.

## Sandbox image

- Node 20, `pnpm` (corepack), `git`, `bash`, the `claude` CLI (pinned),
  `supabase` CLI (for schema-touching tdd tickets). `gh` is **not** needed in the
  image — push/PR are host-side (ADR-0002).
- Entrypoint: set git committer identity, trust the repo, `corepack enable`. It
  does **not** run `gh auth setup-git` or wire HTTPS push — the container has no
  `GH_TOKEN` and never pushes.
- **Persistent pnpm content-addressed store** mounted as a Docker volume so the
  per-worktree `pnpm install` (run several times per ticket — worktree create,
  review prep, gate re-run) is a near-instant hardlink op instead of a network fetch.
- **Commit identity:** `Herd Agent Harness <agent-harness@beherd.co>`. The
  skills' existing `Co-Authored-By: Claude` trailer stays.

## Stop control

- **Graceful, per-ticket checkpoint.** Stop only ever lands *between* tickets
  (after a full implementation→review→retrospective cycle), never mid-ticket —
  stopping between stages would strand a half-finished worktree.
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

- Per-session wall-clock caps: **implementation 30 min, review 15 min,
  retrospective 10 min** (configurable). On expiry the harness kills the container
  and treats the session as failed. The cap is enforced against the **wall clock**,
  not a monotonic timer, so time the host spent asleep counts toward it — a Go
  `time.AfterFunc` freezes during macOS sleep and once let a container that lost
  its API stream mid-sleep hang for two days (BEH-386 class).
- **Idle heartbeat (`SESSION_IDLE_TIMEOUT_MS`, default 20 min):** alongside the
  hard cap, every session is watched for stream silence. The watchdog taps the
  docker stdout reader; if no bytes arrive for the idle window the stream is
  treated as dead (e.g. the Anthropic connection silently severed while the host
  slept) and the container is killed even though the hard cap may have time left.
  The default is deliberately generous: the agent's stream emits nothing between a
  `tool_use` and its `tool_result`, so one long quiet tool call (a `pnpm run build`
  or `test-storybook` run) is legitimately silent for minutes — set the window too
  low and it reaps a healthy session mid-build. The wall-clock hard cap is the
  backstop, so the heartbeat only needs to detect a dead stream sooner than the cap
  would. Set to 0 to disable.
- **CI watch caps (BEH-414, all configurable):** poll interval 30 s, poll budget
  20 min (one wait for checks to go terminal), auto-fix budget 30 min + max 2 fix
  attempts (the flake re-run is separate and doesn't count). Each auto-fix session
  reuses the review wall-clock cap. On exhaustion the PR + worktree are kept and
  the failing checks are printed.
- **Failure matrix:**

  | Outcome | implementation | review (+ harness gate/push/PR) | retrospective |
  |---|---|---|---|
  | Clean success (ground-truth passes) | commit ahead → run review | gates green → push + PR → CI watch → CI green → run retrospective | `out.json` present → file findings → remove worktree, ticket done, next |
  | Crash / non-zero exit / timeout | log + breadcrumb, skip rest, next | log + breadcrumb, keep worktree (no push), next | log + breadcrumb, keep worktree, next |
  | Ran but ground-truth fails | no commit → skip + breadcrumb | gates **red** → no push, breadcrumb, keep worktree, next; OR PR open but **CI red after auto-fix budget** → keep PR + worktree, print failing checks | `out.json` absent → breadcrumb, keep worktree, next |

- **Circuit breaker:** 3 consecutive ticket failures → stop and report (assume
  something environmental broke, e.g. expired auth or a broken base build —
  rather than burn the whole `agent-ready` queue failing identically).

## Logging

- **Console = concise harness narration**, one timestamped line per event
  (`selected BEH-312 (Urgent)`, `tdd ✓ committed a1b2c3d`, `review ✓ PR #418`,
  `stop requested — finishing current ticket`, `queue empty — exiting`).
- **Disk = full forensic detail, keyed by ticket** (not by run id) so any later
  tool finds a ticket's whole arc by globbing one dir:
  - `agent-harness/logs/BEH-NNN/<session>-<run-id>.jsonl` — the complete
    `claude … --output-format stream-json` transcript per session
    (`implementation-…`, `review-…`, `retrospective-…`). The ticket id is the
    stable key all three tools share; the run-id is a filename suffix for ordering
    and uniqueness when a ticket is worked more than once. This is what lets
    `retrospective BEH-NNN`, invoked separately, locate the implementation and
    review transcripts.
  - `agent-harness/logs/BEH-NNN/findings/retrospective/out.json` — the dropbox.
  - `agent-harness/logs/BEH-NNN/run.jsonl` — the structured event stream
    (machine-readable mirror of the console).
- `--verbose` tees the raw agent stream to the console too; off by default.

## Harness runtime (host side)

- **Go 1.26+**, standard library only — zero module dependencies: `net/http`
  (Linear GraphQL), `os/exec` (driving `docker`), `os`/`encoding/json` (logs +
  stop file). No dep tree on purpose — this process holds real credentials.
  Built with `go build`; the binary is self-contained (no runtime needed on the
  host beyond `docker`).
- Config: `agent-harness/.env` (`LINEAR_API_KEY`, `ANTHROPIC_API_KEY`,
  `GH_TOKEN`, `HERD_PATH`, bot identity, timeouts) + CLI flags (`--verbose`,
  `--once` for a single ticket then exit, label/timeout overrides).

## Worktree lifecycle

- Created by the `tdd` skill inside the sandbox (`scripts/new-worktree.sh beh-nnn feat`).
- Persists on the host between sessions via the bind mount, and — because of the
  real-path mount (ADR-0002) — is fully usable from the host: a human can `cd` in
  and run git on it.
- **On a clean run (review pushed + retrospective filed) → `git worktree
  remove`** (branch is pushed and the PR captures everything; the worktree is pure
  disk cost). The harness runs this **host-side** via the main checkout — the
  real-path mount makes the worktree's `.git` pointer resolve on the host, so no
  throwaway container is needed.
- **On any failure → keep it** (recoverable artifact a human or a re-run can pick
  up).
