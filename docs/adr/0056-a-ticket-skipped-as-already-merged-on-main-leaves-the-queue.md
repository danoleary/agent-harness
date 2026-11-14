# ADR-0056: A ticket skipped as already merged on main leaves the queue

- Status: Accepted
- Date: 2026-11-14
- Amends: ADR-0052 (adds a Disposition, AlreadyOnMain, settled the same way as ResolvedOnMain)

## Context

Before it claims or launches anything, the implementation stage skips a ticket
whose key a recent `origin/main` commit names (`TicketAlreadyOnMain`, BEH-528).
The skip returned a bare `OK`. That read as a successful implementation, so the
pipeline ran review, which failed because no worktree existed. The retrospective
then ran over the host checkout and filed its findings again. The run folded to
NoPR, so the loop released the ticket to Todo and counted a breaker failure. A
ticket the tracker had left in Todo after its PR merged was selected again within
seconds, pass after pass, until the breaker tripped with "fix the environment and
relaunch". Nothing in the environment was wrong (#331, BEH-1391).

## Decision

**The skip is its own Disposition, `AlreadyOnMain`. It ends the slice, and the
loop hands the ticket to a human instead of releasing it.**

- **The stage.** `TicketAlreadyOnMain` returns the newest commit that names the
  key. The skip returns `OK` with `AlreadyOnMain`, the commit's full SHA in
  `Result.ResolvedBy` and its subject in `Result.ResolvedWhy`. `--force` still
  dispatches anyway.
- **The pipeline.** No session ran, so the slice ends at implementation, as it
  does for BaseRed: no review and no retrospective. It exits 0.
- **The loop.** It keeps the claim, labels the ticket blocked and comments the
  commit, asking a human to close the ticket, as for ResolvedOnMain (ADR-0052).
  It does not release the ticket.
- **The breaker.** AlreadyOnMain is neutral.
- **The reap.** The retrospective removes the worktree only if one exists. Before,
  a run that never made one logged "worktree kept — removal failed … is not a
  working tree".

## Considered

- **Move the ticket to Done.** The ticket asked for this. The tracker port has no
  Done transition: `MoveToCanceled` is the only terminal one, and Canceled would
  record merged work as not planned. The match is also weaker than a merged PR
  linked to the ticket. A commit that only mentions the key ("follow-up to #12")
  makes it too, which is why `--force` exists. So the harness does not close the
  ticket. It labels it blocked, the existing label for "a human must act", which
  is the ticket's own fallback (it named `needs-info`). Closing it stays a human's
  call, with the commit on the ticket.

## Consequences

- A merged ticket stuck in Todo costs one pass, not three passes and a breaker
  trip. No review or retrospective session is launched for it.
- The ticket stays In Progress and blocked until a human closes it or removes the
  label. A false positive is visible on the ticket and in the run log, and
  `--force` dispatches it.
