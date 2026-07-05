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

The eventual destination is an autonomous loop over the `ready-for-agent` queue (see
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
   To keep the "recommend close" verdict from firing on valid tickets (BEH-629),
   extraction drops prose git-trailer keywords (`Fixes`/`Closes`/`Resolves`) and
   known third-party package internals (e.g. `sentryFunctionMiddlewareHandler`),
   and grades the verdict: a symbol named in an add/introduce context (`Add a
   `beforeSend``) is one the ticket exists to *create*, so its absence downgrades
   to a soft "verify premise" note — only a missing symbol the ticket implies
   *already exists* keeps the strong "recommend close" wording.
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
  `--next` (the *single-shot* tool) logs `no eligible ticket — queue empty` and exits 0.
  The long-running **loop** treats the same empty-queue condition differently — it
  *idles and re-polls* rather than exiting (§The loop) — because a daemon's job is to
  wait for the next ready ticket, not to end. Both read "no ticket" from the same
  side-effect-free `selectNextTicket()`; only the reaction differs (exit vs idle). The
  distinct log line carries the "nothing to do" signal either way.

## The loop

```
startup:
  fetch + fast-forward origin/main on the primary checkout
  reset consecutive-failure counter

loop:
  if stop requested (Ctrl-C or STOP file)   -> exit cleanly

  --- disk reclaim (host-side, between tickets, ADR-0005) ---
  if statfs(worktrees volume).free < DiskReclaimThreshold:   // cheap probe every iter; default ~8 GiB (> the 5 GiB sandbox floor)
    run: scripts/prune-merged-worktrees.sh --yes             // removes ONLY merged-PR + clean worktrees (gh-aware); never main/dirty/no-PR
    if still < threshold -> run: pnpm store prune            // cheap, non-destructive, network-free follow-up
    narrate "reclaimed N worktree(s), freed X" only if it acted
    # non-fatal: a failed prune (gh down / ENOSPC) is logged and the loop continues — NOT a ticket outcome, never touches the breaker

  ticket = selectNextTicket()               // Linear GraphQL, harness-owned
  if no ticket:                             // daemon: empty queue is IDLE, not done
    log "queue empty — idle, will re-poll"
    sleep pollInterval in short ticks, checking stop each tick  // responsive stop while idle
    continue                                // re-poll; a newly ready-for-agent ticket is picked up

  Linear: move ticket -> In Progress         // harness owns ALL Linear I/O

  --- host-side worktree provisioning (BEH-636) ---
  git worktree add -b <branch_prefix>/beh-nnn <worktrees_dir>/beh-nnn (based on origin/main)  // harness owns creation
  post_create hook: throwaway container runs the Consumer's toolchain setup in the worktree  // herd: env links + pnpm install + Playwright. warn-only.
  # retires the sandbox-runs-new-worktree.sh coupling: git creation is host-side, toolchain is a declared config hook

  --- implementation: /tdd (sandbox, 30 min cap) ---
  run: claude -p "/tdd Work on BEH-NNN. The worktree at <worktrees_dir>/beh-nnn on <branch_prefix>/beh-nnn already exists — cd in; do NOT run new-worktree.sh. <injected ticket context>"
  impl OK <=> worktree exists AND <branch_prefix>/beh-nnn has >=1 commit ahead of merge-base(origin/main)
  if not OK -> log + Linear breadcrumb comment + skip rest + record failure + continue

  fetch + fast-forward origin/main           // "pull main after every session"

  --- review prep: repopulate web/node_modules before the cold session (BEH-490) ---
  throwaway install container: `pnpm install --frozen-lockfile` in the worktree
  # handoff strips node_modules (BEH-412); pre-install so the session doesn't pay it mid-gate. warn-only.

  --- review: /review-worktree (sandbox, 25 min cap) ---
  run: claude -p "/review-worktree <worktree-path>  <injected ticket context + 'do not touch Linear; commit locally ONLY — do NOT push, do NOT run gh; do NOT emit findings (retrospective owns that)'>"
  # agent has no GH_TOKEN; it can only commit into the shared local .git

  --- review ground truth + push gate (harness, host-side) ---
  re-run each config-declared named gate (BEH-634) in its own throwaway container, in
    order, on feat/beh-nnn — herd's are `pnpm run check` then `pnpm run typecheck`; a Go
    Consumer's would be `go test ./...`. Stops at the first red and reports it by name.
  docs-only short-circuit (BEH-687): if the branch's net diff against origin/main touches ONLY
    documentation/prose paths no gate reads (root markdown, `docs/**` — NOT anything under a module
    source tree like `web/`, NOT the skill trees `.agents/`/`.claude/` whose SKILL.md files feed the
    agent-harness skill-contract CI job, and NOT scripts/workflows/migrations/manifests), SKIP the heavy host gate
    re-run entirely (oxlint + oxfmt over ~1200 files + tsgo validate nothing a prose edit could break —
    the PR #743 waste). Treated as a green gate. Read fresh at each gate call so a code edit committed by
    an earlier session flips it off; fail-safe to running the full gate on any git doubt. The qualitative
    review + clean-tree + mergeability gates still apply — only the compile/format gate is skipped.
  review OK <=> ALL gates are GREEN AND worktree is clean AND the review session emitted its verdict
                AND that verdict's disposition is NOT `blocked` (BEH-580)
                AND the branch makes a NON-empty net diff against origin/main (BEH-603)
                // never the agent's self-report; a green gate is NOT a review (BEH-569)
  if review has NO verdict but exited CLEANLY (code 0, not an OOM/cap-abort) over a clean,
     gate-green worktree (BEH-624): the diff is byte-identical and already verified — the review
     just stopped a turn short of printing its verdict. Re-launch the review session in-stage,
     BOUNDED (initial + 1 re-launch), re-gating the possibly-rewritten tree each time, before
     falling through to fail-closed. The cheapest incompleteness class to recover — mirrors the
     OOM / conflict-resolution re-launch pattern; a persistent no-verdict still fails closed and
     keeps the worktree. (An OOM 137, a cap abort, a dirty tree, or a red gate are NOT eligible.)
  if recommend-close (BEH-603): worktree clean AND `git diff origin/main` is EMPTY
                // a zero-net-diff branch (the empty-commit BEH-365 produced) — checked before the
                // gate/review checks, after the clean-tree gate (a dirty tree may hide uncommitted work)
    -> NO push, NO PR: this is a correct terminal no-op, not a failure. The implementation +
       review correctly declined to ship an empty commit and concluded the ticket is a
       duplicate/superseded. Surface a Linear breadcrumb recommending the ticket be CLOSED,
       KEEP the worktree for audit, and return the recommend-close disposition. The loop keeps
       the ticket In Progress for a human to close (NOT released to Todo — it must never re-loop
       to the same conclusion) and the breaker treats it as neutral. (Fixes the PR #642 mistake.)
  if OK     -> git -C <worktree> rebase origin/main   // BEH-570: replay onto the fresh base
                 - clean replay  -> continue (the long pipeline let main move; PR opens current)
                 - content conflict -> NO dead-end (BEH-581): launch a sandboxed conflict-resolution
                     session over the worktree (rebase + resolve + commit, like the CI auto-fix),
                     verify ground truth (clean tree AND branch actually rebased — not abort-to-stale),
                     re-run the host gate on the resolved tree; only then push. If it can't land a
                     clean, re-gated rebase -> abort + KEEP worktree + Linear breadcrumb (a spending-cap
                     abort defers quietly). A genuine unresolvable conflict still ends with a human.
               git -C $HERD_PATH push origin feat/beh-nnn
               gh pr create --repo <origin> --head feat/beh-nnn --base main \
                            --title <templated> --body <templated: ticket id + commit subjects>
  if not OK -> log + Linear breadcrumb comment + KEEP worktree + record failure + continue
               # incl. verdict-absent (spending-cap abort / OOM): fail closed, never push an unreviewed diff
               # incl. verdict-blocked (BEH-580): review ran + found an unresolved Blocker/Important
               #   finding it couldn't self-resolve → fail closed, keep worktree for a human decision

  --- review CI watch + auto-fix (harness, host-side, BEH-414) ---
  zero-net-diff short-circuit (BEH-602): BEFORE the first poll, if `git diff origin/main` for the
    pushed branch is EMPTY -> there is nothing for CI to validate that main hasn't already validated,
    so do NOT watch (a no-op PR can never meaningfully go green — the PR #642 ~20-min poll-budget
    waste). Return the recommend-close disposition: keep the PR + worktree, flag the ticket for a human
    to close as superseded. The review push-gate already recommend-closes a zero-diff branch BEFORE the
    push (BEH-603), so this is the BACKSTOP for a branch that became a no-op only AFTER the pre-push
    rebase (a sibling PR merged the same fix during the multi-minute gate), plus any standalone/resumed
    review. Fail-safe: any git doubt reads NON-empty, so a flaky read falls through to the normal watch.
  docs-only short-circuit (BEH-687): BEFORE the first poll, if the pushed branch's net diff against
    origin/main touches ONLY documentation/prose paths no gate or CI job reads (root markdown, `docs/**`;
    NOT the `.agents/`/`.claude/` skill trees, whose SKILL.md files feed the agent-harness skill-contract CI job;
    `gitpkg.BranchDocsOnly`) -> the change cannot break CI, so polling the full job just burns the whole
    poll budget on something that can never fail it (the PR #743 waste: a one-line AGENTS.md edit died to
    the ~12-min poll timeout). PASS immediately as a trivially-mergeable green ship — still routed through
    the same greenOutcome mergeability gate as the wedged-ready pass, so a genuine base conflict is NOT
    shipped. Distinct from the zero-diff case: here the diff is REAL, it just touches nothing a gate reads.
    Fail-safe: any git doubt reads NON-docs-only, so a real code change falls through to the normal watch.
  structurally-wedged required-context short-circuit (BEH-614): a repo whose branch protection requires a
    context that only runs on `merge_group`/`refs/heads/main` (a merge-queue or main-only check) lists that
    context on the PR as `state=EXPECTED` in `gh pr checks` and NEVER schedules a run for it on the PR head,
    so it sits in the pending bucket forever. That is distinct from the zero-diff case above: here the diff
    is real and the gates are green — the PR is legitimately mergeable, it just has a required check that
    cannot run until it reaches the merge queue. The watch detects the wedge structurally (`wedgedReady`):
    the instant EVERY real gate is green and the ONLY thing left pending is one or more `EXPECTED` contexts,
    it short-circuits the poll with `ErrWedgedReadyForMergeQueue` and PASSES as "ready for the merge queue"
    (still gated on mergeability, like any green) — rather than burning the poll budget / stall window on a
    check that will never move on the PR (the PR #661 / BEH-336 ~4.5-min waste that dead-ended in a generic
    "check the PR manually" stall). Safety against a PR-open false positive: it requires at least one REAL
    gate to have gone green AND no genuinely-running (QUEUED/IN_PROGRESS, i.e. non-EXPECTED-pending) check —
    so before the real workflows settle (when contexts can momentarily be EXPECTED with nothing yet green)
    the watch keeps polling. This is structural and immediate, where the older `PollStall` no-progress window
    (BEH-602) was only a slower, generic timeout-class backstop for the same wedge.
  poll `gh pr checks feat/beh-nnn` until terminal (success/failure/cancelled), bounded by a poll budget
  gh-auth/permission degrade classes (NOT a red build — the diff is pushed + gate-green, the harness just
    can't READ CI): these fail SOFT to a green pass that leaves the open PR for a human, distinct from
    "CI did not go green". Two signatures, keyed off `gh pr checks` stderr:
      - 403 "Resource not accessible by …" (fine-grained PAT lacks the Checks permission) -> errChecksUnobservable
        -> pass with "CI status unobservable with this token" (needs a classic repo-scoped PAT; BEH-476)
      - 401 / "Bad credentials" / "gh auth login" hint (the poll path's token is missing/stale/wrong even
        though `git push` + `gh pr create` just succeeded with the harness auth) -> errChecksUnauthenticated
        -> pass with "CI watch unavailable: gh not authenticated … PR was pushed OK, check CI manually and
        fix the harness gh auth" (BEH-627, the BEH-625 false-negative). A future operator seeing a 401 from
        `gh pr checks` should read it as a harness gh-auth problem, NOT a CI failure.
  if green        -> confirm mergeability against base (gh pr view --json mergeable):
                       clean              -> done
                       stale-base conflict (main moved after the push) -> auto-rebase + force-with-lease
                         re-push, await CI re-run, re-check (capped); only a genuine CONTENT conflict
                         is left for a human (BEH-570)
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

  // SPENDING-CAP ABORT is a control-flow signal, NOT a ticket failure (see invariant below).
  // If ANY stage ended in an external Anthropic spending-cap abort:
  if any stage SpendingCapAbort:
    release ticket -> Todo            // no progress was made; don't strand it In Progress
    comment on ticket: cap-abort, auto-resumes after the cap resets (breadcrumb, BEH-590)
    do NOT touch the breaker counter  // not the diff's fault; breaker stays blind to it by design
    long backoff sleep (interruptible by STOP, same as idle; default ~30–60 min)
    continue                          // auto-resume: re-poll after the cap window resets

  fetch + fast-forward origin/main

  // RELEASE-ON-NO-PR (BEH-590): any non-cap run that did NOT reach a pushed PR — OOM,
  // sandbox crash, a review stage that died before pushing, an empty-diff verification
  // failure — left the ticket claimed In Progress with nothing to show. The dispatch
  // claimed it on select, so undo the claim or the board reads "in flight" forever and
  // the dispatch guard never re-grabs it. A shipped PR is the success signal and is
  // never released, even on a non-zero exit (CI red after the auto-fix budget).
  // RECOMMEND-CLOSE (BEH-603) is the one no-PR mode that is NOT released to Todo: the branch
  // makes zero net change, so re-grabbing it would re-run the whole pipeline to the same
  // "nothing to ship" conclusion forever. Keep it In Progress for a human to close.
  if RecommendClose:
    do NOT release            // keep In Progress; a human closes it as a duplicate/superseded
    comment on ticket: empty diff, recommend closing as duplicate/superseded (breadcrumb, BEH-603)
  else if NOT reached a pushed PR:
    release ticket -> Todo            // don't strand it In Progress; a later run re-grabs it
    comment on ticket: run produced no PR, released to Todo (breadcrumb, BEH-590)

  // breaker signal = "did this ticket reach a PUSHED PR?", NOT the pipeline exit code
  if ticket reached a pushed PR -> reset consecutive-failure counter to 0
  else if SpendingCapAbort or RecommendClose -> neutral (neither increment nor reset)
  else                          -> increment consecutive-failure counter
  // a retrospective-only failure (PR shipped) and a CI-red-after-budget (a reviewable PR
  // exists) do NOT count as failures; only "never produced a PR" does.
  // a recommend-close (BEH-603) is a correct terminal no-op — neutral, like a cap abort.
  // an idle/empty-queue tick is neutral — it neither increments nor resets.
  if 3 consecutive ticket failures -> exit + loud report (circuit breaker)
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
  real commit ahead of merge-base **that shares a common ancestor with `main`** — a
  *disjoint* history (empty merge-base, e.g. the 963-commits-ahead
  [BEH-355](https://linear.app/beherd/issue/BEH-355) branch) is a suspect ground
  truth, not a healthy handoff, and fails the gate
  ([BEH-597](https://linear.app/beherd/issue/BEH-597)); *review* = the harness's **own** re-run of the
  gates is green over a clean worktree **and** the review session emitted its
  seven-lens verdict (all three are the push gate — no branch reaches a PR on the
  agent's say-so); *retrospective* = `/findings/out.json` exists on disk (an empty
  `[]` is a valid "ran, found nothing"; an *absent* file means the step never ran
  and is a failure). The agent's own "I'm done" is logged but never authoritative.
  One external hazard would otherwise break this contract
  ([BEH-568](https://linear.app/beherd/issue/BEH-568)): an **external Anthropic
  spending-cap abort** ([BEH-494](https://linear.app/beherd/issue/BEH-494)) —
  distinct from the harness's own time/spend caps. It can strike at session *start*
  (replacing the first assistant turn with a synthetic "Spending cap reached"
  message) or *mid-session* after the skill wrote its up-front default `[]` (the
  BEH-536 incremental write), so a dropbox can be present alongside it yet only ever
  be a stale prior-run file or that early `[]` — never proof a genuine analysis turn
  ran. Trusting it would mask the abort into a false "ran, found nothing" success and
  skip the retry-after-reset. The harness defends in depth, keying off both signals
  (the `is_error` cap result **and** the `model:"<synthetic>"` turn, not the exit
  code alone): the retrospective stage drops the empty default dropbox at the source
  so the *absent* = never-ran contract holds, and `verify.Retrospective` gives the
  cap-abort signal precedence over a *present* `out.json` so the abort wins even if
  that on-disk clear didn't. Either way the run routes to ↻ retry-after-reset; any
  real findings written before the cap fired are preserved and still filed.
- **A review with no verdict fails the push closed ([BEH-569](https://linear.app/beherd/issue/BEH-569)).**
  The host-side gate re-run authorises the push, but a green gate only proves the
  diff compiles — it is **not** a review. A review session killed before it emitted
  its `## Review:` verdict (a spending-cap abort [BEH-494], an OOM [BEH-525]) leaves
  the diff with **zero** qualitative review, yet its gate re-run still runs green
  against the committed handoff. Substituting that mechanical gate for the review and
  pushing would open a PR nobody reviewed while reporting "clear to push" — a silent
  single point of failure. So `verify.Review` gates the push on `ReviewComplete`
  (the verdict was emitted, surfaced by `ReviewQualitative`) **in addition to** green
  gates + a clean worktree: a verdict-absent review is fail-closed — no push, no PR,
  worktree kept so a resumed review can finish before the branch ever ships. The
  BEH-494 retry-after-reset note already fired for the *session*; this is what makes
  the *pipeline* honour it instead of pushing past it.
- **A review that ran but is *blocked* on an unresolved finding fails the push closed ([BEH-580](https://linear.app/beherd/issue/BEH-580)).**
  The companion case to BEH-569: there the review never ran; here it ran, found a real
  Blocker/Important finding, and was structurally unable to act on it. The
  `/review-worktree` skill was written for a human-in-the-loop — it asked "apply the
  fixes? (a)/(b)?" and waited. In the autonomous pipeline (ADR-0002) there is no
  approver, so the review emitted its `## Review:` verdict with the finding still open
  and the harness — reading the verdict as the green "review ran" signal — pushed the
  PR anyway, shipping the unaddressed finding (the BEH-439 leak). The fix is two-sided:
  the skill now **self-resolves** under the harness (apply a safe fix, or
  accept-and-document a deliberate change) and, only for a finding it genuinely cannot
  resolve, declares `Disposition: blocked` in the verdict instead of asking; and
  `verify.Review` gates the push on `ReviewBlocked` (surfaced by `stream.IsReviewBlocked`)
  **in addition to** the BEH-569 conditions — a blocked verdict is fail-closed, keeping
  the worktree for a human decision rather than opening a PR on an unresolved finding.
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
  The **review** stage applies the same safety net ([BEH-559](https://linear.app/beherd/issue/BEH-559)):
  a review session killed mid-edit by a spending cap would otherwise lose its
  in-progress nit fixes on resume — the worktree is re-derived from the committed
  tip — so the resumed review re-judges the original diff and can flip its verdict on
  the very line review-1 had started fixing. Checkpoint-committing the started edits
  (subject names the *review* session) keeps them on `feat/beh-nnn` and visible in the
  resuming review's merge-base diff. As in implementation, the checkpoint is done
  *after* the push decision and never authorises a push: an unverified, half-applied
  fix is preserved, never shipped.
- **A transient sandbox failure is retried, not charged to the ticket ([BEH-542](https://linear.app/beherd/issue/BEH-542)).**
  Two environmental failures look like a session result but aren't the diff's fault:
  a **137 OOM-kill** under host memory pressure, and a **transient exit-125 launch
  failure**. Two exit-125 signatures count as transient engine wedges, not the
  diff's fault: docker's overlay2 store gone read-only (`… read-only file system`)
  when the host disk/IO wedges momentarily ([BEH-542](https://linear.app/beherd/issue/BEH-542)),
  and the container vanishing mid-run under memory pressure so docker's wait stream
  hits EOF (`error waiting for container: unexpected EOF`) — the same OOM class as
  the 137 kill, but it took out the whole container rather than one command, so
  docker reports it as an exit-125 launch failure with no recovery turn left to the
  agent ([BEH-547](https://linear.app/beherd/issue/BEH-547)/[BEH-550](https://linear.app/beherd/issue/BEH-550) —
  a session was killed ~90s in by exactly this, costing the whole ticket with no
  handoff). The EOF match is anchored on "waiting for container" so a bare
  `unexpected EOF` from a config/parse fault never trips it. `session.Outcome.Retryable`
  folds these and the 137 into one predicate (`sandbox.IsRetryableStartFailure`), and
  `session.RetryTransient` (formerly `RetryOnOOMKill`) retries them with a backoff.
  *implementation* wraps its launch in it, so a crash at the
  worktree-creation step — the session's first and heaviest host I/O — recovers on a
  bare retry (a fresh `--name`, the same create prompt: the wedged container's `--rm`
  teardown may have left the name taken, and a creation-time 125 left no worktree to
  resume). A *genuine* 125 (daemon down, image missing, bad flag, or a bare
  `unexpected EOF` from a config/parse fault) stays terminal.
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
    The loop also reclaims *proactively* one tier above this floor: a between-ticket
    `statfs` runs `prune-merged-worktrees.sh` when free space dips below a soft
    `DiskReclaimThreshold` (~8 GiB), so the daemon self-heals merged-worktree creep
    *before* the preflight ever has to refuse a launch ([ADR-0005](adr/0005-loop-reclaims-disk-by-pruning-merged-worktrees.md)).
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
- carries the **`ready-for-agent`** label (the human-in-the-loop blast-radius gate —
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

**Filing dedup (BEH-573).** Before filing each finding, the harness dedups it
against the team's open harness findings in two passes: first an exact `key`/
title match (the fast `matchKey` short-circuit), then — for anything that misses
— a best-effort **semantic** pass that asks a cheap host-side model "is this the
same root-cause class as one of these open issues?". The semantic pass is what
catches the common case the exact match can't: two sessions wording the *same*
failure differently and picking different (or empty) free-form keys. On any match
(either pass) the existing issue is bumped as a recurrence — an occurrence comment
plus a bumped `<!-- occurrences: N -->` body marker — instead of filing a
duplicate, turning N reworded dups into one issue carrying N occurrences. Both new
paths are best-effort (ADR-0001): the model needs a host-side `ANTHROPIC_API_KEY`
(a subscription OAuth token is rejected on the `x-api-key` header, BEH-316), and a
missing key, a model error, or a Linear error all degrade to the prior exact-match
behaviour with one narration line — never a crash, never a re-filed duplicate. The
cheap model is set by `DEDUP_MODEL` (default a small Haiku snapshot).

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
ships. The `ready-for-agent` label remains the human gate on *what* runs unattended.

## Sandbox image

The image is split into a **base** (the harness↔sandbox contract) and a
**Consumer layer** (the project toolchain), so a new project adopts the harness by
`FROM`-ing the base and adding its own tools — never by forking the harness
(ADR-0008).

### The base image (the contract surface)

Carries only what the harness↔sandbox contract requires, and nothing project-
specific:

- The `claude` CLI (pinned), `git`, `bash`, `gosu`, `ca-certificates`.
- The **uid-re-exec entrypoint**: it stats the bind-mounted checkout and drops
  from root to the checkout owner's uid via `gosu` before exec'ing `claude`
  (`--dangerously-skip-permissions` refuses to run as root). It sets the git
  committer identity and trusts the repo. It does **not** run `gh auth setup-git`
  or wire HTTPS push — the container has no `GH_TOKEN` and never pushes (ADR-0002).
- The **bind-mount layout**: the workspace checkout at its real host path, the
  optional `/findings` dropbox, and an optional toolchain cache volume.

`gh` is **not** in the image — push/PR are host-side (ADR-0002).

### Consumer image composition (build-or-pull)

A Consumer declares its sandbox image in `.agent-harness/config.toml` one of two
ways; declaring **neither** is a hard error at config load (there is no sane
cross-language default):

- **`dockerfile`** — a checkout-relative path to a Consumer Dockerfile that `FROM`s
  the base and adds its toolchain. The harness **builds** it (`docker build -f
  <dockerfile> <dir>`) on a local image miss.
- **`image`** — a prebuilt, published, base-compatible image ref. The harness
  **pulls** it on a local miss.

`Preflight` (`internal/sandbox`) makes this build-or-pull decision on a miss, then
re-inspects to confirm the tag now exists before committing the run — so a bad
Dockerfile / mistyped ref / missing registry auth fails loud *before* the ticket
is claimed, not as an opaque exit-125 after. herd stays on the build path (it
commits `agent-harness/Dockerfile` and names the local tag `herd-agent-harness:latest`).

### Generalized toolchain cache

The persistent cache is a **Consumer-declared volume name + mount path** (`[cache]`
in config), mounted into every sandbox + gate container so the per-worktree install
(run several times per ticket — worktree create, review prep, gate re-run) is a
near-instant hardlink op instead of a network fetch. It generalizes the old
herd-specific pnpm store: a NuGet (`/root/.nuget/packages`), Go (`GOMODCACHE`), or
pnpm (`/pnpm-store`) Consumer differs only in config. The cache is **optional** — a
Consumer that declares none mounts no cache volume. The deprecated
`pnpm_store_volume` key still maps onto `[cache]` with the historical `/pnpm-store`
mount for back-compat.

- **Commit identity:** `Herd Agent Harness <agent-harness@beherd.co>`. The
  skills' existing `Co-Authored-By: Claude` trailer stays.

### Base-image registry, publishing & versioning (HITL)

**Recommendation (to confirm):** publish the base to **GHCR** at
`ghcr.io/herd-video-call-limited/agent-harness-base`, tagged with an **immutable
semver** (`:0.1.0`, …) plus a moving `:latest`. A Consumer Dockerfile pins the
immutable tag (`FROM ghcr.io/.../agent-harness-base:0.1.0`) so a base bump is a
deliberate, reviewable edit — never a silent floating pull. Publish from a CI
workflow in the (future) standalone harness repo (ADR-0007) on a tagged release,
building `Dockerfile.base` for `linux/amd64` + `linux/arm64`.

> **Deferred to the publish decision (this ADR's HITL):** creating the standalone
> `Dockerfile.base` artifact and rebasing herd's `agent-harness/Dockerfile` to
> `FROM` the published base. A genuine `FROM <published-base>` cannot build until
> the base is actually published (or local base-build orchestration is added,
> which is out of scope), so the split lands with the publish, keeping herd's
> single-Dockerfile build working unchanged in the meantime. The harness code
> contract above (build-or-pull, image-or-dockerfile, generalized cache) is in
> place and ready for it.

## Stop control

- **Graceful, per-ticket checkpoint.** Stop only ever lands *between* tickets
  (after a full implementation→review→retrospective cycle), never mid-ticket —
  stopping between stages would strand a half-finished worktree.
- **Two signals, one check** (`stopRequested || existsSync(STOP_FILE)` at each
  between-ticket checkpoint **and on every idle-sleep tick**):
  - `SIGINT` (Ctrl-C) — flips the flag, logs `will stop after current ticket`,
    leaves the running session alone.
  - sentinel file `agent-harness/STOP` — the one that matters for AFK runs:
    `touch` it from anywhere and the harness winds down after the current ticket.
- **Idle is also a stop point (daemon).** Because the loop is long-running and
  sleeps on an empty queue (§The loop), the idle re-poll sleep is broken into
  short ticks that re-check the stop condition, so a stop requested while idle is
  honoured within a few seconds rather than after a full poll interval.
- **The sentinel is cleared at clean startup.** The loop deletes a pre-existing
  `agent-harness/STOP` when it starts, so a stale sentinel from a prior run can't
  instantly kill a fresh launch; thereafter only a *new* `touch` stops it.
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
  | **Ready for merge queue (BEH-614)** — CI watch finds all real gates green and the only pending check is a structurally-wedged required context (`state=EXPECTED`, merge-queue/main-only) | commit ahead → run review | gates green → push + PR → CI watch **short-circuits the moment the gates are green** (no stall-window/poll-budget waste) → PASS as ready-for-merge-queue (still mergeability-gated) → run retrospective; treated as clean success (breaker resets) | runs as usual |
  | Crash / non-zero exit / timeout | log + breadcrumb, skip rest, next | log + breadcrumb, keep worktree (no push), next | log + breadcrumb, keep worktree, next |
  | Ran but ground-truth fails | no commit → skip + breadcrumb | gates **red** (or dirty worktree, or **no verdict** — spending-cap/OOM, BEH-569, or verdict **blocked** on an unresolved finding, BEH-580) → no push, breadcrumb, keep worktree, next; OR PR open but **CI red after auto-fix budget** → keep PR + worktree, print failing checks | `out.json` absent → breadcrumb, keep worktree, next |
  | **Recommend-close (BEH-603)** — clean worktree, **empty** `git diff origin/main` | (n/a) | zero net change → **no push, no PR**; breadcrumb recommending the ticket be closed as a duplicate/superseded; keep worktree for audit; **kept In Progress** (NOT released to Todo); breaker-neutral | runs as usual |
  | **Recommend-close at the CI watch (BEH-602)** — branch became **empty** only AFTER the pre-push rebase (sibling merged the same fix during the gate), so the PR is already open | (n/a) | CI watch short-circuits BEFORE the first poll (no ~20-min poll-budget waste) → keep PR + worktree; same recommend-close breadcrumb + **kept In Progress**; breaker-neutral | runs as usual |
  | **Docs-only (BEH-687)** — net diff touches ONLY docs/prose paths no gate or CI job reads (root markdown, `docs/**`) | commit ahead → run review | host gate re-run **skipped** (nothing a prose edit could break) → push + PR → CI watch **short-circuits BEFORE the first poll** (no ~12-min poll-budget waste, the PR #743 death) → PASS as trivially-mergeable (still mergeability-gated) → run retrospective; treated as clean success (breaker resets) | runs as usual |

- **Circuit breaker:** 3 consecutive ticket failures → **exit and report
  loudly** (assume something environmental broke, e.g. expired auth or a broken
  base build — rather than burn the whole `ready-for-agent` queue failing
  identically). The trip action is a full daemon exit on the same wind-down path
  as a STOP, not a pause-and-wait: it forces a human to investigate before more
  tickets are consumed, and recovery is a relaunch once the environment is fixed.
  - **"Failure" is "did not reach a pushed PR" — not the raw pipeline exit code.**
    A successful PR (implementation + review + push) resets the counter to 0; a
    ticket that never produced a PR (impl crash/empty diff, review gates red)
    increments it. A **retrospective-only failure** (the PR already shipped) and a
    **CI-red-after-auto-fix-budget** (a reviewable PR exists for a human to take
    over) do **not** count — otherwise three genuinely-hard-but-shipped tickets
    could trip the breaker while the harness is working fine. A **recommend-close**
    (BEH-603 — a zero-net-diff branch correctly concluded to be a duplicate) is a
    correct terminal no-op, so it is **neutral** too (like a spending-cap abort):
    a run of legitimate duplicates must never trip the breaker. An idle/empty-queue
    tick is neutral: it neither increments nor resets.
  - **A "poison" top-of-queue ticket trips the breaker by design (v1).** Because any
    run that did not reach a pushed PR *releases the ticket back to Todo* (BEH-590,
    generalising the no-worktree-crash release of BEH-543), the loop re-selects that
    same top-of-queue ticket on the next iteration and fails identically — so three
    such iterations trip the breaker and the daemon exits. This is intended: a
    ticket that fails 3× in a row is exactly the "a human
    should look" signal, and the cause is usually environmental (full disk, broken
    base build) that would sink the *next* ticket too, not poison specific to that
    id. The breaker bounds the wasted respin to 3 attempts. **The trip report names
    the repeated ticket** when all 3 failures share one id (`BEH-NNN failed 3× —
    start here`) so the human sees the offender immediately. *Deferred follow-up:* if
    real runs show single bad tickets halting otherwise-healthy queues, quarantine a
    ticket after N releases (skip it / drop `ready-for-agent`) so the daemon
    continues — not built in v1.
- **Spending-cap abort backoff (daemon-only runaway guard).** An external Anthropic
  spending-cap abort (`session.Outcome.SpendingCapAbort`, BEH-494) is the one
  runaway the breaker is deliberately blind to: it is classified retry-after-reset,
  **not** a failure, so it never increments the counter — but in a `for {}` daemon a
  capped account aborts *every* session at start, which would spin the loop through
  the whole queue in seconds, producing nothing. So a cap-abort is handled as
  control flow, not a verdict: the daemon **releases the ticket to Todo** (no
  progress to protect), leaves a **breadcrumb comment** on the ticket noting the
  cap-abort + auto-resume (BEH-590), leaves the breaker counter untouched, and enters
  a **long interruptible backoff** (default ~30–60 min) before re-polling, auto-resuming
  once the cap window resets. Parsing the exact reset time from the abort message is
  deliberately *not* done — fragile string-parsing for minutes of saved latency; the
  fixed backoff + re-poll is robust. The backoff is **narrated** (BEH-605): it emits a
  `cap-backoff` entry record naming the duration and expected wake time, a periodic
  "still capped, re-poll ~HH:MMZ" heartbeat (every `capBackoffHeartbeat`, 1 min) while
  it waits, and a wake record on re-poll — so a multi-hour backoff is never mistaken
  for a dead daemon, and `cmd/watch` can render "capped, resuming at HH:MM" instead of
  going blank.
- **Stale-claim reaper (dead-agent cleanup).** The harness moves a ticket to In
  Progress on claim, but an agent that dies before pushing — sandbox OOM, timeout,
  crash — strands it In Progress with **no branch and no PR**, occupying the lane
  forever because the queue reads "started" as "someone's on it" and never re-grabs
  it (BEH-677). Between tickets, before selecting, the loop lists the agent-claimed
  In Progress set (unassigned + `ready-for-agent` + `started`, so a human's In
  Progress work is never a candidate) and **releases a claim back to Todo** — with a
  breadcrumb comment — when **all three** hold: (1) it is older than
  `LOOP_CLAIM_TTL_MS` (default 30 min), (2) it has no linked PR (a Linear `.../pull/`
  attachment), and (3) it has no remote branch (`git ls-remote` word-boundary-matched
  on the key). The **grace TTL is load-bearing**: between claim and first push a
  *healthy* agent looks identical to a dead one (no branch/PR yet — the observed
  mid-flight case was ~18 min), so reaping on the no-branch signal alone would kill
  live work; the TTL is the only thing that tells them apart, so it is never skipped.
  Both I/O signals fail **safe toward not-reaping** (a git error → assume a branch
  exists; a list error → skip the pass and narrate), and the whole pass is
  best-effort like disk reclaim — never a ticket outcome, never touching the breaker.
  Separately, an **umbrella/tracker issue with an open child is excluded from
  selection** entirely (its real work lives in the children; claiming it would only
  produce another stranded In Progress claim).

## Logging

- **Console = concise harness narration**, one timestamped line per event
  (`selected BEH-312 (Urgent)`, `tdd ✓ committed a1b2c3d`, `review ✓ PR #418`,
  `stop requested — finishing current ticket`, `queue empty — idle, re-poll 60s`).
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
  - `agent-harness/logs/BEH-NNN/run.jsonl` — the per-ticket structured event
    stream (machine-readable mirror of the console, scoped to one ticket's arc).
  - `agent-harness/logs/loop.jsonl` — a **global** structured event stream across
    all tickets and stages, written by the shared narration sink and truncated at
    daemon startup (bounded to one daemon run). Each record is `{ts, kind, ticket,
    stage, message, detail}`: `message` is the console line verbatim (a strict
    superset of `loop.log`), and `kind` is a closed enum the **viewer** switches on
    without re-parsing prose. This is the daemon→viewer contract (ADR-0005).
- `--verbose` tees the raw agent stream to the console too; off by default.

### The viewer (`cmd/watch`)

The daemon runs detached, so its only window is `loop.log` — noisy and
unstructured. The **viewer** is a separate, **read-only** binary that tails
`logs/loop.jsonl` and renders a live dashboard: the current ticket, the stage
(n of 3), the current step, daemon health, and a stateful ASCII mascot (labelled
`status:`) whose mood (working / waiting / sleeping / celebrating / hurt / stopping /
stopped) follows the last event `kind` — except that **process liveness, not the
event stream, is authoritative for "stopped"**: a dead daemon (clean exit or unclean
death) flips the mascot to `stopped`, freezes the animation, and raises a loud banner
above it — red for a stop (its reason if the loop wrote a terminal record, else
"exited without clean shutdown") and amber for a `STALLED` suspicion when a live
daemon has gone quiet past a threshold (`WATCH_STALL_AFTER`, default 10m); a terminal
bell rings once on the transition into either state. See ADR-0006. Health also
reflects two read-only probes beside the stream: a present `STOP` sentinel reads as
"winding down" (and flips the mascot to `stopping`), and an absent stream file on a
live daemon reads as "no event stream" (distinct from a present-but-empty "no events
yet"). It is **stdlib-only** (a redraw-on-a-ticker dashboard needs no TUI framework)
and degrades to plain scrollback when stdout is not a TTY, `NO_COLOR` is set, or
`--no-animation` is passed; `--no-bell` suppresses the transition bell (as does
`NO_COLOR`). It never
controls the loop — `touch agent-harness/STOP` remains the only control path (the
viewer only *reads* the sentinel), and quitting the viewer does not touch the daemon.
"Progress" is honest about being indeterminate: a stage indicator and a tool-call
activity counter, never a percent-complete bar. See ADR-0005 for why a separate
reader over a structured stream rather than a `--tui` flag on the daemon.

## Harness runtime (host side)

- **Go 1.26+**, standard library only — zero module dependencies: `net/http`
  (Linear GraphQL), `os/exec` (driving `docker`), `os`/`encoding/json` (logs +
  stop file). No dep tree on purpose — this process holds real credentials.
  Built with `go build`; the binary is self-contained (no runtime needed on the
  host beyond `docker`).
- Config: `agent-harness/.env` (`LINEAR_API_KEY`, `ANTHROPIC_API_KEY`,
  `GH_TOKEN`, `HERD_PATH`, bot identity, timeouts) + CLI flags (`--verbose`,
  `--once` for a single ticket then exit, label/timeout overrides).
- **`cmd/loop` config knobs** (host-side, following the existing `*_MS` env
  convention; all optional with the defaults below):

  | Env | Default | Meaning |
  |---|---|---|
  | `LOOP_POLL_INTERVAL_MS` | `60000` (60s) | idle re-poll wait on an empty queue, broken into ~2–5s ticks that re-check the stop condition |
  | `LOOP_CAP_BACKOFF_MS` | `2700000` (45 min) | backoff after a spending-cap abort before re-polling (interruptible by STOP) |
  | `LOOP_MAX_CONSECUTIVE_FAILURES` | `3` | circuit-breaker threshold (consecutive no-PR tickets → exit + report) |
  | `LOOP_MAX_TICKETS` | `0` (unlimited) | optional ceiling: stop after N *attempted* tickets |
  | `LOOP_MAX_RUNTIME_MS` | `0` (unlimited) | optional ceiling: stop after T wall-clock |
  | `LOOP_DISK_RECLAIM_THRESHOLD_BYTES` | `8589934592` (8 GiB) | soft free-disk floor below which the loop prunes merged worktrees between tickets (ADR-0005); `0` disables reclaim |
  | `LOOP_CLAIM_TTL_MS` | `1800000` (30 min) | grace period after which an In Progress claim with no branch/PR is reaped back to Todo (§Stale-claim reaper) |
  | `STOP_FILE` | `agent-harness/STOP` | sentinel path; cleared at clean startup, `touch` to wind down |

  `LOOP_MAX_TICKETS`/`LOOP_MAX_RUNTIME_MS` default to **unlimited** because the loop
  is deliberately long-running (§The loop) — they exist as opt-in insurance for an
  AFK overnight run, switchable without code changes. The loop takes no required
  args (contrast `pipeline`, which always wants a ticket or `--next`); `--verbose`
  forwards to every stage.
- **Run model: a standalone detached process — no tmux, no supervisor.** The loop
  is started as a plain background process the operator walks away from (a thin
  `scripts/loop-start.sh` does `nohup bin/loop >> … & echo $! > agent-harness/loop.pid`),
  not under tmux, launchd, or systemd. Lifecycle is the stop model in §Stop control:
  `touch agent-harness/STOP` to wind down gracefully (the PID file is for a hard
  `kill` only if needed). No supervisor means no auto-restart — a crash or a breaker
  trip stays down until the operator relaunches, which is the intended behaviour for
  the breaker (a human must look first). launchd/systemd is explicitly **deferred**;
  if ever added, it must not auto-restart a clean exit.
- **Exit-code contract (observability, supervisor-agnostic).** `0` = a *deliberate*
  terminal stop — STOP requested, circuit breaker tripped, or an optional
  `LOOP_MAX_*` ceiling reached. Non-zero = an *unexpected* crash (panic, docker
  daemon gone, config error). With no supervisor nothing acts on this, but it lets
  the operator (or a future launchd `SuccessfulExit=false`) tell "stopped on
  purpose" from "died".

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
