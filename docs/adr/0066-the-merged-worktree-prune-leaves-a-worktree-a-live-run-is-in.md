# ADR-0066: The merged-worktree prune leaves a worktree a live run is in

- Status: Accepted
- Date: 2026-11-22
- Amends: ADR-0027 ("Between tickets, never mid-flight")
- Extends: ADR-0064

## Context

ADR-0027 lets the loop run the Consumer's `prune-merged-worktrees.sh` under disk
pressure. The script removes every worktree whose PR has merged and whose tree
is clean. ADR-0027 reasoned that this could never touch live work: the prune
runs between tickets, and "the in-progress ticket's worktree can't match
merged".

Both halves of that stopped holding. Several loops now share one checkout, so
one loop's "between tickets" is another's mid-run. And a ticket's retrospective
runs after its PR is open: once the PR merges, the worktree the session is
reading is merged and clean. In herd a retrospective session lost its worktree
about a minute in (#404). Its next Bash call failed with `Working directory ...
was deleted`, and the run log said nothing, so the session could not tell a
harness cleanup from a sandbox fault.

ADR-0064 closed the PR loop's reclaim with the worktree's run lock. The prune
was the other host-side remover, and it never read a run lock.

## Decision

1. **The prune runs with every live run's worktree locked.** Before the loop
   runs the script, it lists the checkout's worktrees and asks each one's run
   lock (`locks/runs/<slug>.lock`, `loophost.Worktrees.RunLive`) whether a run is
   in it. A live one gets `git worktree lock` with the reason `agent-harness: a
   live run is in it, …`, and the loop lifts the lock when the script exits.
   `git worktree remove` refuses a locked worktree unless forced twice, so the
   script's own classification stays the single source of truth (ADR-0027). The
   lock only takes live runs out of its reach.
2. **Prunes are serialized, and a killed daemon's lock is lifted.** The prune
   holds `locks/prune.lock`, so one loop cannot lift its locks while another
   loop's prune is still running. A lock with the shield's reason found under
   that lock is left from a daemon that was killed mid-prune. It is lifted if no
   run is in the worktree any more, and otherwise kept for this prune and lifted
   after it. A lock taken for any other reason is left alone.
3. **A failed read prunes nothing.** A run lock that cannot be read, a worktree
   list that fails, or a lock git refuses fails the prune. The loop narrates
   that as the usual reclaim warning and goes on to the cache and Docker prunes.
4. **The retrospective names a worktree removed during its session.** Nothing in
   the run removes the worktree before the retrospective's own reap. If the
   worktree was on disk when the session started and is gone when it ends, the
   stage logs a `⚠` line in run.jsonl naming the worktree and the branch that
   still has the change. A remover the harness cannot stop, such as a human or
   an older binary, is still reported in the run's own log.

## Alternatives considered

- **Skip the prune while any run is live.** Disk pressure builds up while runs
  are live, and a busy herd always has one, so the prune would never run.
- **Pass the live worktrees to the script.** The script belongs to the Consumer,
  and every Consumer's copy would have to learn the flag. The script already
  calls `git worktree remove`, which already respects git's own lock.
- **Launch the retrospective in the primary checkout and point it at
  `git -C <primary> log <base>..<branch>`** (#404's second option). The session
  would then load the host checkout's skills and rules, not the branch's
  (ADR-0046). That is what #384 re-creates the worktree to avoid.

## Consequences

- A merged ticket whose retrospective is still running keeps its worktree until
  the retrospective reaps it, or until the next prune after the run ends.
- If a retrospective finishes during a prune, its reap meets a locked worktree,
  logs `worktree kept — removal failed`, and leaves the worktree to the next
  prune. Its PR has merged, so it is reclaimable then.
- A script that removes worktrees with `--force --force`, or with `rm -rf`, gets
  past the lock. The retrospective's `⚠` line still names the removal.
