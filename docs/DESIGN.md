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
   It also logs an *advisory* (never a skip) when the ticket cites code symbols
   that no longer exist in `web/src` (`git.ResolvedAdvisory`) — the BEH-544
   signal that the work likely already merged, often under a *sibling* ticket the
   own-key scan above can't catch. This stays advisory because the symbol signal
   is heuristic (a cited symbol can be absent because the ticket asks to *create*
   it); the `/tdd` prompt separately steers the in-session agent to verify the
   premise still holds and recommend close rather than fabricate a no-op change.
   A third, complementary advisory (`git.ResumedBranchAdvisory`, BEH-554) fires
   when the ticket's *own* `feat/<slug>` branch already carries un-merged commits
   referencing it — a *resumed* worktree whose prior session already landed a
   complete fix. The merge-base with `main` is stale, so `TicketAlreadyOnMain`
   sees nothing and `ResolvedAdvisory` stays quiet when the fix *added* code. It's
   advisory (a resumed branch can hold *incomplete* work too), so instead of
   skipping, the host swaps the `/tdd` prompt for `prompt.BuildTddResumedBranch`,
   which steers the agent to inspect the branch's existing commits (`git log
   main..HEAD`) and prefer verify-and-handoff over re-implementing.
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

## Single-shot auto-select (`--next`)

`pipeline --next` runs the same single-ticket chain as `pipeline BEH-NNN`, but
**selects** the ticket instead of taking it as an argument: it resolves the
top-of-queue eligible ticket via **Ticket selection** (below), runs implementation
→ review → retrospective over it *once*, then exits. It is the smallest increment
that removes the "the human must name a ticket" step — and the seam **The loop**
later reuses, a loop being `--next` selection in a `for {}` with stop control and a
circuit breaker around it. There is no continuous run, no auto-fix, no circuit
breaker here; a human is still at the trigger, pulling it once per ticket.

```
pipeline --next:
  ticket = selectNextTicket()          // Linear read + claim — see "claim-on-select" below
  if no ticket -> log "no eligible ticket — queue empty"; exit 0   // empty queue is not a failure
  run the pipeline over ticket (exactly as pipeline BEH-NNN, with PreClaimed set)
```

Design decisions, and why:

- **An explicit `--next` flag, not bare `pipeline`.** Auto-select **mutates Linear**
  (it claims the chosen ticket → In Progress) and burns a sandbox — too surprising a
  side effect to trigger by *omission*. Bare `pipeline` with no ticket keeps today's
  friendly `usage:` error rather than silently grabbing a ticket the user never named.
  `--next` together with an explicit `BEH-NNN` is a conflict and is rejected — you
  either name a ticket or ask for the next one, never both.
- **Claim-on-select (dequeue semantics).** `selectNextTicket()` claims the ticket
  (Todo → In Progress) **as part of selection**, before returning. This is what makes
  selection a *dequeue*: a second selection (a stray concurrent `--next`, or the
  future loop's next turn) sees the ticket is no longer Todo and picks the next one,
  so two selections can't both grab the same top ticket. The window shrinks to a
  single GraphQL round-trip. The cost — claiming *before* Docker preflight, which
  inverts the implementation stage's hard-won "never claim a ticket we cannot work"
  ordering — is paid back by **release-on-preflight-failure**, and the whole trade-off
  is recorded in [ADR-0003](adr/0003-claim-on-select.md).
- **`PreClaimed` threads the claim state into the stage; the hand-passed path is
  untouched.** Selection sets `args.PreClaimed = true`. The implementation stage then
  (a) skips its own `MoveToInProgress` (the ticket is already In Progress — it would
  be a redundant no-op) and (b) if its **Docker preflight fails**, calls
  `ReleaseToTodo` so the dequeued-but-unworkable ticket returns to the queue instead
  of stranding In Progress. On the hand-passed path `PreClaimed` is false: preflight
  still precedes the claim exactly as before, nothing to release, BEH-316 ordering
  intact. The `--next` path carries the new complexity; the tested path does not move.
- **`--dry-run` performs no Linear *mutations* — but it does *read*.** `--next
  --dry-run` runs the real `selectNextTicket()` query to resolve the actual
  top-of-queue ticket and prints the plan for it, but **never claims** (no
  `MoveToInProgress`). The documented guarantee tightens from the hand-passed
  pipeline's "touches no Linear" to "**mutates** no Linear": selection is
  side-effect-free, so previewing *which* ticket `--next` would grab, and the plan it
  would run, is honest and useful. One log line marks it: `dry-run — selected BEH-NNN
  (not claiming)`.
- **Empty queue exits 0.** No eligible ticket is a normal steady state, not a failure;
  `--next` logs `no eligible ticket — queue empty` and exits 0. This matches what the
  loop already does (`if no ticket -> exit cleanly`), so the loop wraps `--next`
  without remapping exit codes. The distinct log line carries the "nothing to do"
  signal for any future cron without overloading the process exit code.

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

  --- review: /review-worktree (sandbox, 25 min cap) ---
  run: claude -p "/review-worktree <worktree-path>  <injected ticket context + 'do not touch Linear; commit locally ONLY — do NOT push, do NOT run gh; do NOT emit findings (retrospective owns that)'>"
  # agent has no GH_TOKEN; it can only commit into the shared local .git

  --- review ground truth + push gate (harness, host-side) ---
  re-run gates in a throwaway container: `pnpm check && pnpm typecheck` on feat/beh-nnn
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
        - escape hatch (BEH-561): if all gates reproduce green and the red is a
          cancelled/superseded/flaky run (no code defect), the agent commits NOTHING
          rather than fabricating a speculative diff; the harness then adds an empty
          commit so the re-push gives CI a fresh HEAD to re-run against
      harness pushes the fix (or the empty re-trigger commit); re-poll
    on green   -> done
    on exhaust -> non-success: KEEP PR + worktree, print failing checks + logs pointer

  fetch + fast-forward origin/main

  --- retrospective: /retrospective (sandbox, 45 min cap) ---
  precondition (host-side, BEH-553): SKIP if feat/<slug> does NOT resolve to a git revision AND no implementation-*/review-*.jsonl transcript exists under logs/BEH-NNN/
    -> a retrospective with neither input could only emit a masking [] or a self-referential "no inputs" finding; skip before paying the cap. One real input (a failed slice has transcripts but maybe no branch) is enough to run.
  run: claude -p "/retrospective for BEH-NNN. Read every transcript under logs/BEH-NNN/ + the diff. Append harness/environment findings to /findings/out.json (ALWAYS write the file, even as []; write it EARLY and update as you go so a late OOM/kill can't lose it). Touch no code, no Linear."
  retro OK <=> /findings/out.json EXISTS          // absent & not killed => never ran; absent after a 137 kill => killed-before-write, retry
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
- **A transient sandbox failure is retried, not charged to the ticket ([BEH-542](https://linear.app/beherd/issue/BEH-542)).**
  Two environmental failures look like a session result but aren't the diff's fault:
  a **137 OOM-kill** under host memory pressure, and a **transient exit-125 launch
  failure** — docker's overlay2 store gone read-only (`… read-only file system`)
  when the host disk/IO wedges momentarily. `session.Outcome.Retryable` folds both
  into one predicate, and `session.RetryTransient` (formerly `RetryOnOOMKill`) retries
  them with a backoff. *implementation* wraps its launch in it, so a crash at the
  worktree-creation step — the session's first and heaviest host I/O — recovers on a
  bare retry (a fresh `--name`, the same create prompt: the wedged container's `--rm`
  teardown may have left the name taken, and a creation-time 125 left no worktree to
  resume). A *genuine* 125 (daemon down, image missing, bad flag) stays terminal.
- **A first-stage crash that leaves no worktree is re-attempted once, then releases the claim ([BEH-543](https://linear.app/beherd/issue/BEH-543)).**
  When implementation fails with **no worktree ever created** (the transient retries
  above exhausted, or a kill before any work) there is nothing to salvage. The stage
  flags this outcome `Retryable` (`retryableEnvCrash` — no worktree *and* not a
  spending-cap abort) on its `Result`, and the **pipeline re-attempts the whole
  implementation stage once** before giving up: such a crash strikes at the
  worktree-creation step's heavy host I/O and is usually transient, so a fresh attempt
  may get further. A genuine run-to-completion empty diff is *not* `Retryable` and is
  never re-attempted. The re-attempt is bounded to one extra try so a persistently
  sick host (e.g. a full disk) can't spin the slice. Whether or not the re-attempt is
  taken, the no-worktree branch still releases the claim back to **Todo**
  (`linear.ReleaseToTodo`) so a later run re-grabs it instead of leaving the ticket
  stranded *In Progress* — the one deliberate exception to "failure never mutates
  Linear state" below, safe precisely because there's no partial state. (A worktree
  that *does* exist is kept + checkpoint-committed, never released or re-attempted.)
- **A full host disk fails fast as a precondition, and degrades to a warning where it can't ([BEH-540](https://linear.app/beherd/issue/BEH-540)).**
  Host disk exhaustion is the root cause behind two otherwise-baffling signatures —
  a **mid-session exit-125 with `driver "overlay2" failed to remove root filesystem:
  unlinkat …: read-only file system`** (the daemon's overlay2 store goes read-only
  under disk/IO pressure; *not* a Docker config bug) and a **findings-dir `mkdir …:
  no space left on device` (ENOSPC)**. Two guards, prevention then graceful-degrade:
  - **Prevention (the precondition):** `sandbox.Preflight` checks free space on the
    checkout volume *first*, before any `docker` call, and refuses to launch below
    `MinFreeDiskBytes` (5 GiB — well above new-worktree.sh's 2 GiB *warning*, which
    proved too low: BEH-540's session was warned at 1282 MiB free and ran and died
    anyway). The error names the free space, the path, and the reclaim steps (`pnpm
    store prune`, `scripts/prune-merged-worktrees.sh`), so the disk is caught *before*
    the ticket is claimed rather than as an opaque exit-125 overlay2 teardown after.
    A statfs error is non-fatal — an unreadable probe doesn't block a launch.
  - **Graceful-degrade (where prevention can't reach):** a findings-dir `mkdir` that
    still races to ENOSPC is recognised (`isDiskFull`) and logged as a clear,
    actionable warning rather than a hard pipeline error that masquerades as a stage
    crash and forces a manual re-run.
  An exit-125 whose reason *is* the read-only-fs signature is also retried as transient
  (BEH-542 above); the precondition is the cheaper front-line defence that stops the
  session ever starting on a doomed disk.
- **The harness owns all remote I/O — Linear ([ADR-0001](adr/0001-harness-owns-linear-integration.md))
  *and* git push / PR ([ADR-0002](adr/0002-harness-owns-remote-io.md)).** Agents
  commit only into the local shared `.git`; the harness pushes (via the main
  checkout) and opens the PR host-side. The sandbox is air-gapped except for
  Anthropic: the **only** secret in the container is the Claude credential
  (`ANTHROPIC_API_KEY` *or* `CLAUDE_CODE_OAUTH_TOKEN`). No `GH_TOKEN`, no Linear.
- **Failure never mutates Linear state and never deletes a worktree** — with one
  narrow exception: an implementation crash that left **no worktree at all** releases
  the claim back to Todo (BEH-543 above), because there is no recoverable artifact to
  protect. Otherwise it logs, drops a Linear breadcrumb comment, and continues;
  worktrees are the recoverable artifact and are never deleted on failure.
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

`selectNextTicket()` implements this predicate. Its **first consumer is `pipeline
--next`** (see "Single-shot auto-select") — not only the future loop — and it
**claims** the chosen ticket as part of selection (dequeue semantics,
[ADR-0003](adr/0003-claim-on-select.md)), so the returned ticket is already In
Progress.

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

**Precondition — those inputs are also a launch gate (BEH-553).** The `out.json`
post-check tells "ran, found nothing" (`[]`) apart from "never wrote" — but it
cannot tell either from "there was never anything to study", and it only learns
the inputs are missing *after* burning a full sandbox. So the dispatch path now
gates the launch on the inputs: it skips **only if BOTH** are absent — no
`implementation-*.jsonl`/`review-*.jsonl` transcript exists under `logs/BEH-NNN/`
(`stages.hasUpstreamTranscripts`) **and** `feat/<slug>` does not resolve
(`git.BranchExists`). One real input is enough to run. Otherwise it skips with a
`retrospective skipped` diagnostic and reports a clean no-op (OK, never a pipeline
failure) — the BEH-318 shape, where a 30-min retro was dispatched against a ticket
whose log dir held only its own stream and whose branch never existed. The skip is
deliberately BOTH-absent, not either-absent: the pipeline runs the retrospective
even on a *failed* slice ("exactly the run worth mining"), which routinely has
transcripts but no branch — skipping on a missing branch alone would suppress those.
The decision is the pure `verify.RetrospectivePreconditions`.

**Output — the findings dropbox**, kept deliberately simple and out-of-band:

- A dedicated writable dir is mounted at a fixed container path (`/findings`;
  host `logs/BEH-NNN/findings/retrospective/`), **outside the repo tree** so it
  never dirties the worktree or risks being committed.
- The retrospective prompt instructs it: append findings to `/findings/out.json`
  as a JSON array of `{title, body, kind}`; do **not** file Linear issues; and
  **always write the file, even as `[]`** (so absence means the step never ran).
- After the session returns, the harness reads `out.json`, files one Linear issue
  per finding (team BeHerd, referencing the worked ticket), and logs each.

**Re-run dedup context (BEH-539).** When a ticket goes through the pipeline more
than once, an earlier run's retrospective may have already filed findings. The
harness already skips re-filing a finding whose `key` has an open issue, but a
naive re-run session has no signal those classes are settled — so it burns its
budget re-deriving them. Before launching the session, the harness gathers the
already-filed classes (the team's open harness findings **plus** any classes left
in this ticket's prior dropbox, read before it's cleared) and injects them into
the retrospective prompt with an instruction to treat them as settled and look
only for **new** friction. Best-effort: a Linear lookup failure degrades to the
dropbox classes alone. A first run has nothing to inject and reads as before.

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

- Per-session wall-clock caps: **implementation 30 min, review 25 min,
  retrospective 45 min** (configurable via `RETROSPECTIVE_TIMEOUT_MS`). The
  retrospective gets the largest cap of the three because it is a read-heavy step
  that parses several large jsonl transcripts; it previously borrowed the 30 min
  tdd cap and was killed mid-read before it could write findings (BEH-536). Every
  cap is kept **above the 20 min idle window** (below), so the idle/no-progress
  watchdog can reap a stalled session before the hard cap rather than at it — the
  review family's old 15 min cap sat below the idle window, leaving its idle
  watchdog inert so memory-pressured review/install/gate sessions burned to the
  cap (BEH-535/538). On
  expiry the harness kills the container and treats the session as failed. The cap is enforced against the **wall clock**,
  not a monotonic timer, so time the host spent asleep counts toward it — a Go
  `time.AfterFunc` freezes during macOS sleep and once let a container that lost
  its API stream mid-sleep hang for two days (BEH-386 class). The cap is *evaluated*
  by a 15 s poll ticker, which is itself monotonic and freezes during sleep, so the
  kill lands on the first tick after wake — the observed wall-clock kill time can
  exceed the cap (a 30 min cap once landed at 66 min because the host slept ~36 min,
  reading as a broken timer). Two refinements keep that honest (BEH-538): when both
  the cap and the idle window are past, the kill is attributed to whichever deadline
  came **first** (a session idle since minute 5 is reported as a dead stream, not a
  late cap), and the kill log surfaces the wall-vs-monotonic gap as `host slept ~36m`
  so a late kill is never mistaken for a cap-enforcement bug.
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
  - `agent-harness/logs/BEH-NNN/<step>-<run-id>.log` — a **raw-stdout step log**
    for the non-agent commands the review tool runs around the session: the
    `install-…` worktree prep and the host-side `gate-…` re-run. These are piped
    `pnpm` output (NOT a stream-json event stream), hence `.log`, never `.jsonl`,
    so a reader doesn't expect parseable JSON. Each ends with a self-describing
    `-- step exited <code> … --` footer (`runlog.StepFooter`) so an OOM-kill is
    visible at the tail rather than an opaque truncation needing a `run.jsonl`
    cross-reference (BEH-537).
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
