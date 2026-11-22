# ADR-0061: A re-run on a branch that stands on its verified handoff starts at review

- Status: Accepted
- Date: 2026-11-22
- Extends: ADR-0016 (verify gathers its own ground truth), ADR-0053 (a ticket whose last run found no work waits)

## Context

A re-run of a ticket always started with a new implementation session, even
when the branch already held finished work. In one herd run (#367), the branch
held a handoff an earlier session left and the harness verified. A later session
on the same branch confirmed the work was complete and added one more commit.
The harness then started a third implementation session on the same branch. It
re-ran the narrow gates, changed no code, and used a full sandbox cap.

BEH-554's `ResumedBranchAdvisory` only steers that session to verify and hand
off. It never skips the session, because a resumed branch can hold incomplete
work: a recovery checkpoint, a half-finished slice, a human's commit. The
neighbouring case, "already fixed on `origin/main`", has its own outcome
(ADR-0052, ADR-0053). Nothing recorded "complete on this branch".

## Decision

**The implementation stage records the HEAD of each handoff it verified. A
re-run whose branch still stands on that commit launches no implementation
session and starts at review.**

1. **The record** is `handoff.json` in the ticket's log dir, beside the review's
   `review-verdict.json`. It holds the HEAD the session left. The stage clears it
   before it launches a session, and writes it only when that session exited 0
   and `verify.Tdd` passed its handoff. It therefore only ever describes the
   latest session: a later session that crashed, capped, was interrupted or
   failed verification leaves no record.
2. **The skip** runs after the claim and before provisioning. It reads the
   record and then git itself, as ADR-0016 requires, and holds only when:
   - the worktree exists, is clean, and its branch is ahead of `origin/main`;
   - the record names a commit the branch still carries
     (`Worktree.SubjectsSince`);
   - every commit after it is a harness recovery checkpoint (`git.IsCheckpoint`);
   - the last review's host Gate re-run (`review-verdict.json`) did not go red
     over it or over the current HEAD. `verify.Tdd` passes a handoff on its
     commits alone, so a handoff whose own Gate is red is not finished work, and
     starting at review would fail on the same Gate every re-run.
3. **When it holds**, the stage logs `tdd ⏭ skipping the implementation session
   — <branch> stands on the verified handoff <sha> …` and returns OK, so the
   pipeline runs review and the retrospective as for a fresh handoff. Review
   re-provisions the worktree through `post_create` itself, so neither
   `post_create` nor the base gate runs in the skipped stage.
4. **Anything else** starts at implementation, as before: no record, an
   unreadable record, a commit after the handoff, a dirty tree, a branch a
   rebase rewrote (it no longer carries the recorded commit), or a red host
   Gate over the handoff.

## Alternatives considered

- **Read the verdict from the previous run's log.** `run.jsonl` is narration, not
  a contract, and a later run's lines interleave with it.
- **Follow a rebased handoff by patch-id.** The pre-push rebase rewrites every
  SHA. Following the handoff through it would save one more session in that
  case, but would also have to tell a clean replay from a conflict resolution's
  edits (ADR-0035). The rewritten branch starts at implementation, which is what
  happened before this ADR.

## Consequences

- A re-run over finished work costs a record read and two git reads, not a
  sandbox cap.
- A handoff the last session left, that review then refused, is reviewed again
  rather than re-implemented. A blocked verdict waits on a human's answer on the
  ticket, which the new review reads.
- Deleting `handoff.json` from the ticket's log dir makes the next run start at
  implementation.
