# ADR-0064: A worktree a live run is in is never reclaimed

- Status: Accepted
- Date: 2026-11-22
- Amends: ADR-0043 (decisions 2 and 3)

## Context

ADR-0043 made the run lock the sign that a run is live. The PR loop removes a
leftover worktree that holds a PR's branch when the worktree is clean, origin
has its HEAD, and the ticket loop does not hold the branch. The hold
(`TicketSource.HeldBranches`) is an agent-claimed In Progress ticket whose run
lock is held, so it starts from the tracker's claim list. A live run whose
ticket is missing from that list holds nothing. Linear's list, for example, only
includes unassigned tickets that still carry the ready label.

In herd this happened twice (#384). The review stage's CI watch gave up on a red
PR and logged `keeping PR + worktree`. A few seconds later the retrospective
warned that the worktree was not on disk. It started its session in the host
checkout, which loaded that checkout's stale skills, while the branch still
stood at the reviewed head. The red verdict that ended the watch also ended
pr-doctor's `checks pending` skip. The worktree was clean and pushed, and the
holds did not name the branch, so the PR loop removed the worktree from under
the run. The log line saying the worktree was kept was true when it was
written. Something outside the run made it false.

ADR-0063 took the CI watch out of the ticket loop's review stage. That shortened
the window but did not close it: a PR that goes red between the push and the end
of the retrospective, or a conflict, still exposes it.

## Decision

1. **The PR loop reads the worktree's own run lock before it reclaims it.** A
   ticket's worktree is `.claude/worktrees/<slug>` and its run lock is
   `locks/runs/<slug>.lock`. Before the PR loop judges a foreign worktree safe to
   remove, and again just before removing it, it asks whether that lock is held
   (`prloop.Worktrees.RunLive`, `loophost.Worktrees`). A held lock means the PR is
   skipped with `branch checked out in worktree <path>, which a live run is in`,
   reported in the stream once, and no attempt is spent. A lock that cannot be
   read is also a skip. The holds from the claim list stay as they are. This is
   a second check that does not depend on the tracker. A worktree that no run
   ever locked, such as a human's, reads as not live, and ADR-0043's rules still
   apply to it.
2. **The retrospective re-creates a missing worktree from its branch.** If the
   worktree is gone but its branch is not, the retrospective re-attaches a
   worktree to the branch as it stands, narrates it, and launches its session
   there rather than in the host checkout. If that fails, for example because
   another worktree has the branch checked out, it says so and falls back to
   the host checkout as before. The reap afterwards treats the re-created
   worktree like any other.
3. **Review names a kept worktree that is gone at its end.** Every way out of
   review keeps the worktree. So a worktree missing when the stage returns was
   removed by something outside the run, and review logs a `⚠` there, where it
   happened.

## Alternatives considered

- **Drop the claim list and hold every branch whose run lock is held.** The
  claim list also tells the PR loop which ticket holds the branch, and it reads
  the run lock by ticket, not by worktree. Checking the worktree's lock as well
  covers the gap and leaves that alone.
- **Change the log message to say the worktree may be removed.** The run did
  keep the worktree. The bug was another loop removing it, and a reworded
  message would only describe the bug.
- **Fail the retrospective when its worktree is gone.** The retrospective's
  value is highest on a run that went wrong, and the branch still has
  everything the session needs.

## Consequences

- A PR whose ticket run is live waits for that run to end, even when the
  tracker no longer lists the ticket as claimed. Its red CI or conflict is
  fixed on a later poll.
- A run from a binary that predates the run lock still holds none, as
  ADR-0043 says, so its worktree can still be reclaimed. The retrospective's
  re-creation now covers that case too.
- The retrospective's re-creation fetches origin/main best-effort, as every
  `Worktree.Create` does.
