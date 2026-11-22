# ADR-0070: A session reports a block or a follow-up ticket in a marker, and the host settles both on the tracker

- Status: Accepted
- Date: 2026-11-22
- Amends: ADR-0052 (adds a second marker beside `resolved.json`, and a Disposition, SessionBlocked)

## Context

A sandboxed session cannot reach the tracker (ADR-0010). Its one typed way to
report an outcome other than a handoff commit was `/findings/resolved.json`, for
a ticket already fixed on `origin/main` (ADR-0052). Two other outcomes had no
channel, and the herd consumer lost both (#368):

1. **Blocked on a human.** The ticket's premise is false, or it asks for a
   decision the session cannot make. The session recorded the block in an empty
   handoff commit. The harness read that as a handoff, sent it to review and
   later re-queued the ticket, and the next session stopped on the same block.
2. **A follow-up ticket is needed.** A ticket says to split the work into two
   pull requests when a condition holds. One branch per ticket means the session
   ships the first half; the second lived only in the handoff text and the PR
   body, and nobody filed it.

## Decision

**The session writes `/findings/blocked.json`; the host comments and labels a
block, and files each follow-up as a ticket behind a human.**

- **The marker.** Every implementation envelope (fresh, resumed branch, after a
  refusal) documents it next to `resolved.json`:
  `{"reason": "...", "questions": ["..."], "follow_ups": [{"title": "...", "body": "..."}]}`.
  The host clears any marker an earlier run left before each session, as for
  `resolved.json`. A marker that is not that JSON is narrated and read as none.
- **A block.** A non-empty `reason` makes the run **SessionBlocked**, carrying
  the reason in `Result.BlockedWhy` and the non-blank questions in
  `Result.BlockedQuestions`, whatever the branch holds: an empty handoff commit
  was the old way to record a block, and review would ship it. A
  `resolved.json` that holds wins over it. The stage logs
  `tdd ⊘ blocked on a human — the session stopped: <reason>`. It is not OK,
  so the pipeline skips review and pushes nothing, but the slice did its job, so
  the pipeline and the standalone `implementation` exit 0. The retrospective
  still runs.
- **The loop.** It keeps the claim, applies `tracker.blocked_label` through
  the existing `Block` hand-off (#230), and posts the reason and questions as one
  comment. The selector already skips a blocked-labelled ticket, so a human
  removing the label is the only signal that it may be worked again; comments or
  edits alone do not count, and no "changed since the block" state is kept. The
  breaker treats it as neutral, as it does ResolvedOnMain.
- **Follow-ups.** Each entry with a title files whatever the verdict, through
  the findings filing path (`internal/filing`): a project-audience finding with
  `needs_human`, so it files `Ready: false`, with `RelatedKey` the worked ticket
  so it links back. The host writes them as the dropbox and files it, as it does
  its red-base finding; the session's own `out.json` is still never filed
  (#336).

## Consequences

- A blocked ticket leaves the ready queue with the session's reason and
  questions on it, instead of costing a review and a second session.
- A split's second half becomes a ticket a human makes ready, instead of a line
  in a PR body.
- Follow-ups take the findings path's dedup. A re-run that names the same
  follow-up records a recurrence on the open issue rather than filing a twin,
  but the semantic matcher can also fold a follow-up into an open issue it reads
  as the same class.
- A session can still block a ticket it could have worked. The block is the
  session's word; the label puts it in front of a human, who removes it to have
  the ticket worked again.
