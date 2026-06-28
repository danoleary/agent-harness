# ADR-0005: The autonomous loop reclaims disk by pruning merged worktrees

- Status: Accepted
- Date: 2026-06-28

## Context

The loop daemon (ADR-0004) runs unattended for days, working ticket after ticket.
Each ticket's pipeline spins up a sandbox in which the agent runs
`scripts/new-worktree.sh`, creating `.claude/worktrees/<slug>` on the host — and
**every worktree carries its own heavy `node_modules`**, GBs apiece. On success the
worktree is torn down, but on a failed/kept review, a crash, or a cap-abort it stays
on disk, and nothing ever reclaims it. Worktrees therefore accumulate monotonically
across a long run (the host had 18 lying around when this was written).

This is the BEH-382 failure mode: the disk fills, and the symptom is *not* a disk
warning — it surfaces as an opaque failed `Edit`/test or, for the daemon, a sandbox
that won't launch. `internal/sandbox` already refuses to start a session below a
`MinFreeDiskBytes = 5 GiB` hard floor; once the daemon crosses that floor every
subsequent ticket fails the same way, and with no human watching the loop just burns
the queue producing nothing — exactly the unattended-rot ADR-0004 set out to avoid.

A reclaimer already exists: `scripts/prune-merged-worktrees.sh` removes worktrees
whose PR has **merged** (squash-merge-aware via `gh pr view`) and whose tree is
**clean**, never touching `main` or a dirty worktree. It is tested
(`prune-merged-worktrees.test.sh`) and, until now, only ever run by a human. The
question is whether — and how — the daemon should run it itself.

## Decision

**The loop reclaims disk between tickets by shelling out to the existing
`prune-merged-worktrees.sh`, gated on a free-disk threshold.**

- **"Dead" is narrow and reused verbatim.** A reclaimable worktree is one whose PR
  has merged *and* whose tree is clean — the exact contract the script already
  enforces. This is the only category where removal can never lose work (it is in
  `main`). Closed-but-unmerged, no-PR/orphaned (the BEH-590 released-to-Todo case),
  and merely-stale worktrees are **out of scope** — never auto-removed by the
  daemon, because "the ticket went back to the queue" does not mean a human or a
  resumed run is done with the worktree. The loop does not invent a looser
  definition than the script.
- **Threshold-gated, proactive.** At the top of each iteration the loop does a cheap
  `statfs` of the worktrees volume; only when free space is below a soft threshold
  (`DiskReclaimThreshold`, a new `cmd/loop` knob defaulting to ~8 GiB — the 5 GiB
  `MinFreeDiskBytes` floor plus headroom; `0` disables) does it shell out to the
  prune script. The soft threshold sits *above* the hard floor so reclamation
  happens *before* the sandbox preflight would refuse a launch — disk pressure
  self-heals instead of becoming a failed ticket. When disk is healthy the loop
  makes no `gh` calls at all.
- **Worktree prune first, store prune second.** The named goal is dead worktrees, so
  the script runs first. If still under threshold afterwards, the loop runs
  `pnpm store prune` as a cheap, non-destructive, network-free follow-up (it
  recovered ~945 MB in the BEH-382 incident).
- **Between tickets, never mid-flight.** The check runs after the prior outcome is
  folded into the breaker and before the next ticket is selected/launched, when no
  sandbox is active. The in-progress ticket's worktree can't match "merged" anyway,
  so there is no path to clobbering live work.
- **Reclaim is not a ticket outcome.** A failed prune (gh unreachable, `ENOSPC`
  mid-prune) is logged/narrated and the loop continues; it never touches the circuit
  breaker (which keys only on "did the ticket reach a pushed PR?", ADR-0004). This
  follows the BEH-540 precedent of degrading `ENOSPC` to a warning rather than a
  hard failure.
- **Narrate only when it acts** — `loop — reclaimed N worktree(s)` with bytes freed;
  silent when disk is healthy or nothing qualifies.

## Alternatives considered

- **Prune every iteration, unconditionally.** Keeps disk lowest and is the simplest
  control flow. Rejected because the script makes one `gh pr view` per worktree, so
  unconditional pruning puts a network-API storm and its latency on the hot path
  before *every* ticket even when the disk has hundreds of GB free — cost with no
  benefit. The `statfs` gate makes that cost conditional on actual pressure.
- **React only at the 5 GiB hard floor.** Wait until the sandbox preflight is about
  to refuse, then prune and retry the launch. Rejected as strictly worse: by the
  time you're at the floor you may have already failed a ticket, and a single
  merged-worktree removal might not even clear the floor — whereas a soft threshold
  above the floor reclaims with headroom to spare and keeps launches from ever
  blocking.
- **Auto-remove the orphaned / no-PR worktrees too** (the real source of long-run
  creep). Rejected for an *unattended* daemon: those worktrees hold work that is not
  in `main`, and an automated removal can silently destroy a partially-good attempt
  a human would have wanted to resume. The daemon stays conservative; broadening the
  definition is a deferred decision, not a v1 default.
- **Reimplement the merged/clean classification in Go.** Avoids shelling out and the
  bash/`gh` dependency on the host. Rejected to keep a single source of truth: the
  script is already the human-facing reclaimer, is squash-merge-aware, and is tested
  by its `.test.sh`. Two implementations of "is this worktree safe to delete?" would
  inevitably drift, and drift here means lost work. The host already holds
  `GH_TOKEN` and runs `gh` (ADR-0002), so shelling out adds no new boundary.

## Consequences

- `cmd/loop` gains a disk-reclaim step and a `DiskReclaimThreshold` knob; the
  single-shot `pipeline` path is untouched.
- The daemon now invokes `gh` (via the script) on the host during a run — only under
  disk pressure, and consistent with the host-side remote-I/O boundary of ADR-0002.
  Agents inside the sandbox are unaffected.
- Reclamation is conservative by construction: it can only ever remove worktrees
  whose work is already merged to `main`. The long-run orphaned-worktree creep is
  *mitigated but not eliminated* — those still require a human (or a future, opt-in,
  more-aggressive policy). This is called out so a future reader knows the disk can
  still slowly fill from no-PR worktrees over a very long run.
- Because reclaim never trips the breaker, a persistently failing prune cannot stop
  the loop — it will keep working tickets while narrating the failure, until the
  hard floor eventually blocks launches (the existing, visible failure mode).
