# ADR-0053: A ticket whose last run found no work waits for main or the ticket to move

- Status: Accepted
- Date: 2026-11-14
- Extends: ADR-0003 (claim-on-select), ADR-0032 (selection screens)

## Context

An implementation session can conclude there is nothing to do. In #330 the fix
was already on `origin/main` from a different PR that never closed the ticket.
The session exited 0 with no handoff commit, so the run was NoPR. The loop
released the ticket to Todo, and about 20 seconds later it selected the same
ticket again: a claim, the worktree, `post_create`, a second implementation
session and a second retrospective, all ending in the same nothing. Neither the
ticket nor `origin/main` had changed. Unattended, the loop repeats this until a
human closes the ticket.

BEH-603's RecommendClose does not cover it. That disposition is the review
stage's verdict on a branch with an empty net diff. Here no review runs, because
implementation produced no commit.

## Decision

**When an implementation session runs to completion and leaves nothing to hand
off, the stage records the `origin/main` SHA it worked against and the ticket as
selection read it. Selection passes over the ticket while both still stand.**

1. **The record** (`internal/nowork`) is one file per ticket under
   `.agent-harness/locks/no-work/<slug>`. Every loop instance of the Consumer
   shares it, like the base verdicts (ADR-0036). It holds the base SHA, the
   ticket's fingerprint and the time.
2. **The fingerprint** is a hash of the ticket's title, body and sorted labels as
   `SelectNextTicket` returned them. Selection reads it on the dispatching
   selection, and it reaches the stage as `Args.Selected` through the Work
   item's Context. It is taken there because `FetchTicket` returns no labels and
   the claim changes the ticket's state. A hand-passed run has no selection, so
   it records nothing.
3. **What counts as no work**: the session exited 0 after real turns (not a cap,
   a refusal, a cap-kill or a $0 run), and the worktree is clean with no commit
   ahead of `origin/main`. Any other end of the stage clears the record, so it
   only ever describes the latest run.
4. **The screen** (`pipeline.SkipNoWork`) wraps the tracker's selection, inside
   the overlap screen. It reads the record of each ticket it is offered. When
   one has a record, it fetches `origin/main` first, because the idle loop does
   not fetch and would never see main move. If `origin/main` in the primary
   checkout is still the recorded SHA and the
   fingerprint matches, it logs `skipping #N: last run found no work against
   origin/main <sha>; unchanged` and selects past the ticket. The skip streams to
   `loop.jsonl` once as `item-skipped`. A poll whose every ready ticket is
   skipped is a held queue (#281). The screen only reads, so `--dry-run`
   selects through it too.
5. **It fails open.** A record that cannot be read, or a main that cannot be
   read, dispatches the ticket as before this ADR.

## Alternatives considered

- **Park the ticket for a human when the session says it is already resolved.**
  This would need the host to parse the session's final message, which is
  self-report (ADR-0016 trusts only ground truth). The session may also be
  wrong. With the record, the ticket waits until something changes, and a
  human can still close it.
- **Keep the record in the loop's memory, as the red-base hold does
  (ADR-0039).** The record would be lost on restart, would not apply to
  `pipeline --next` or another instance, and the loop would need the ticket's
  fingerprint anyway.
- **Compare the worktree's merge-base instead of `origin/main`.** The worktree
  is reused across runs, so its merge-base does not move when main does.

## Consequences

- An unchanged ticket on an unchanged main costs one file read per poll rather
  than a whole implementation and retrospective cycle.
- A session that found nothing by mistake is not retried until main moves or a
  human edits the ticket. To retry it sooner, edit the ticket, run
  `pipeline <KEY>` by hand, or delete its record file.
- While a ticket is skipped it stays in the ready queue with the NoPR release
  breadcrumb on it. The `skipping` line in the log is how a human finds it.
