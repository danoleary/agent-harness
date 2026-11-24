# ADR-0073: A resolved-on-main ticket waits for close, not for main

- Status: Accepted
- Date: 2026-11-24
- Extends: ADR-0053 (a ticket whose last run found no work waits for main or the ticket to move), ADR-0052 (a session's already-resolved-on-main stop is handed to a human)

## Context

ADR-0053's no-work record stands only while `origin/main` is on the SHA the
run worked against. That suits a run that found no work for a reason main
might change. It does not suit a run whose verdict was that main already
fixes the ticket (`ResolvedOnMain`, or the pre-dispatch `AlreadyOnMain`
skip). In #453 a session found the ticket fixed on main and named the commit.
The loop recommended close, and the ticket stayed open for a human. Then main
moved by one unrelated docs commit, and the screen dispatched the ticket
again. The second run cost a base gate (about 70s), an implementation session
and a retrospective, and it reached the same verdict with the same commit.
Until a human closes the ticket, every merge to main buys another such run.

The loop labels such a ticket blocked, which the tracker's selection skips.
The incident happened where the label did not hold the ticket: no
`blocked_label` configured, or the label could not be applied.

## Decision

**A no-work record that names the commit resolving the ticket stands while
`origin/main` still contains that commit and the ticket reads the same,
however far main moves.**

1. **The record** gains an optional `resolved_by`, the full SHA of the
   resolving commit. The implementation stage writes it for a selected run
   whose session found the ticket `ResolvedOnMain`, and for one its own-key
   scan skipped as `AlreadyOnMain`.
2. **The screen** (`pipeline.SkipNoWork`) runs
   `git merge-base --is-ancestor <resolved_by> origin/main` on such a record.
   While main has the commit and the fingerprint matches, it logs `skipping
   <key>: already resolved on main by <sha>; awaiting close` and passes over
   the ticket. A poll that passes over only such tickets is held as `N
   ticket(s) already resolved on main, awaiting close`.
3. **It dispatches again** when a human edits the ticket, or when main no
   longer has the commit (a revert that rewrote history, a force-push). An
   ancestry check that cannot run fails *open*, as a main that cannot be read
   already does.
4. **Labelling the ticket blocked clears its record.** Once the loop's
   blocked label lands, the label holds the ticket. A human removing it is the
   documented way to say the commit does not fix the ticket. The removal
   restores the labels the record's fingerprint was read with, so a kept
   record would pass the ticket over for as long as main has the commit. The
   record therefore only holds a ticket the label could not hold.

## Alternatives considered

- **Treat a resolved record as permanent until the ticket changes.** A revert
  or a history rewrite would then leave the ticket parked on a fix that is
  gone.
- **Keep the record after the blocked label lands.** Removing the label would
  no longer dispatch the ticket. The human would also have to edit the ticket
  or delete the record file, and neither the loop's breadcrumb nor #368's
  reasoning tells them to.

## Consequences

- An open resolved ticket the label cannot hold costs one file read and one
  local `git merge-base` per poll, not a base gate, a session and a
  retrospective per merge to main.
- A session that named the wrong commit is not retried until a human edits
  the ticket, removes the blocked label after the loop applies it, or deletes
  `.agent-harness/locks/no-work/<slug>`.
