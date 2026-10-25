# ADR-0029: A ticket that names files in flight waits for them

- Status: Accepted
- Date: 2026-10-25
- Extends: ADR-0003, ADR-0023

## Context

The ticket loop dispatched tickets that change the same files at the same time.
Between 18 and 25 Oct 2026 the herd Consumer ran about 15 interactive sessions that
only fixed conflicts between harness PRs. The PRs were siblings under one umbrella
(Document backdrop and page-turn speed). None depended on another, so none
declared a "blocked by" relation, and none named another's PR in its body. They
only touched the same few files: `host-document-session.ts` conflicted 20 times
and `document-page-controls.tsx` 14 times.

ADR-0023 holds a ticket behind a PR its body names. #74 resolves a conflict after
it happens. Neither stops two independent tickets from editing one file in
parallel.

## Decision

**Selection defers a ready ticket whose predicted files overlap the work in flight.
It does this on every tracker, and re-checks on every poll.**

- **Work in flight** (`TicketSource.inFlight`): each open PR whose head is on
  `branch_prefix` (not a fork), with the files GitHub lists for it, both sides of a
  rename included. Then each claimed ticket's worktree, with what it has changed
  since it left `origin/main`, committed or not (`git diff --merge-base`). A
  ticket mid-run has no PR yet, and a second loop instance must still see its
  work. A human's branch is not the harness's work and is not read.
- **Prediction** (`overlap.Predict`): the paths, bare file names and backticked
  module names in the ticket's title and body. A path names itself, every file
  under it, and any file it is the tail of. A file name names every file with its
  stem, so `controls.tsx` also names `controls.test.tsx` and
  `controls.stories.tsx`. A backticked name names files with that stem and the
  files in a directory with that name (a Go package, a TS module). A URL, a
  host-qualified path and a bare word in prose name nothing.
- **Deferral** (`pipeline.DeferOverlaps`): when one piece of work shares
  `OVERLAP_THRESHOLD` (default 1) of the predicted files, selection passes over
  the ticket and logs `#97 deferred: overlaps PR #81 on <files>`. It then selects
  the next ready ticket. To make that possible, `SelectNextTicket` takes the Keys
  to skip, and all three adapters honour them. Nothing on the ticket changes, so
  the next poll re-checks it, and it dispatches on the first poll after the
  overlapping PR merges or closes. A ticket's own branch is never work it waits
  on.
- **Escape and off switch**: a ticket labelled `OVERLAP_ESCAPE_LABEL` (default
  `parallel-ok`) is never deferred. `OVERLAP_THRESHOLD=0` turns the check off.
  Selection now carries the ticket's labels (`ticket.Ticket.Labels`).
- **Fail open**: if the work in flight cannot be read, the check is skipped for
  the poll, with a narration. A wrong dispatch costs a conflict, which a human
  fixes today. A stalled queue costs every ticket.

`agent-harness pipeline --next` selects through the same screen.

## Alternatives considered

- **Add the files the ticket's siblings (same parent) already changed.** The
  ticket proposed this. It needs a parent read on every tracker port, and siblings
  that "name the same module" are already caught by their own text. Left for a
  later change if body mentions prove too sparse.
- **Fail closed, like the blocker read.** A blocker is a statement that the ticket
  cannot be worked. An overlap is only a forecast of a conflict. Stalling the
  whole queue on a GitHub blip to avoid one forecast conflict is the worse trade.
- **Diff the remote branches with git instead of reading the PR's files.** That
  needs a fetch of every head on every poll. A squash-merged branch also keeps
  its diff against an old merge-base, so it would block until someone deletes the
  branch. "Open PR" is the signal that clears when the work lands.
- **Hold the ticket with a label or a tracker relation.** Either needs a write
  and someone or something to undo it. A skip that is recomputed on each poll
  clears itself.

## Consequences

- Each poll costs one PR list, one files read per open harness PR, one claims
  read and one `git diff` per claimed worktree. These happen only while the check
  is on and a ticket is ready.
- The prediction is literal. A ticket that describes a change without naming a
  file or module is not deferred, and dispatches as it did before.
- A bare file name or module name can over-match a common stem (`index.ts`,
  `utils`). The cost is a delay until that PR lands, never a lost ticket. Use the
  escape label when a human knows two tickets are independent.
- A ticket deferred behind a PR that stays open indefinitely waits indefinitely,
  as under ADR-0023. Each poll's log line names the PR.
