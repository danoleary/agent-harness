# ADR-0023: A ticket waits on an open PR its body names

- Status: Accepted
- Date: 2026-10-25
- Extends: ADR-0003, ADR-0010

## Context

Ticket selection holds a ticket only behind a dependency declared as a tracker
relation (GitHub "blocked by", a Linear `blocks`). In #82 the ticket was step one
of a design record that was still an open, unmerged sibling PR, named only in
the body. Nothing told the implementation session the prerequisite was unmerged,
or where it was. The session searched every ref to find the design, then judged
the two PRs disjoint. They were not: both edited one paragraph of a shared design
doc. The sibling merged about 15 seconds after this ticket's PR opened. The CI
watch's reactive rebase could not replay the branch, so it parked a cold-reviewed,
gate-green PR for a manual rebase.

## Decision

**A prerequisite is an open pull request the ticket's body names. Selection waits
for it, the envelope names it, and the CI watch resolves its merge once.**

- **Selection (GitHub adapter).** A ticket whose body names a same-repo `#N`, or a
  `github.com/<owner>/<repo>/(pull|issues)/N` URL, that resolves to an *open pull
  request* is skipped (`#82 skipped: builds on open PR #81`). It stays
  ready-labelled and is picked up once the PR merges or closes, like the native
  blocked-by skip. The ticket's own number and its task-list children are not
  references. A reference that 404s holds nothing, so a stale number cannot wedge
  the queue. Any other read error fails the selection closed, as the native
  blocker read does.
- **Envelope.** `FetchTicket` resolves each such PR to a `ticket.Prerequisite`
  with its head branch. Every Implement variant names the branch and says `main`
  does not have it yet. It tells the session to base an edit to a shared file on
  that branch's version (`git show origin/<branch>:<path>`), or to leave shared
  docs alone. This covers the tickets that are dispatched anyway: a hand-passed
  Key, or a PR opened after selection.
- **CI watch.** `hostio.CIWatch.Resolve` is called when the reactive rebase hits a
  content conflict. If true, the host force-with-lease re-pushes and the watch
  re-polls CI and re-reads mergeability, as it does after a clean rebase. The
  review stage supplies it only when its fetch of the ticket listed a
  prerequisite. It re-fetches the ticket, and only if a prerequisite is no longer
  listed (it merged) does it run the existing conflict-resolution session and the
  host gate (`gate-cirebase-*`), once per watch. Any other conflict parks as
  before.

## Alternatives considered

- **Hold a ticket behind any open item named in prose.** "See also #12" is
  common and would hold work behind unrelated, long-lived issues. An open issue
  that really blocks belongs in the native relation. An open PR is the case the
  relation cannot model.
- **Attribute the conflict to the prerequisite's merge commit.** That needs the
  merge commit's SHA and a merge-base walk, and it reads wrong after a squash
  merge. "Open when the review fetched it, gone now" is the signal the tracker
  already gives, and the cost of a wrong guess is one bounded session.
- **Always offer a resolution session to the CI watch.** That would spend a session
  on every post-PR conflict. #82 asked for it only when the prerequisite merged.

## Consequences

- Linear and Jira do not detect prose prerequisites. Their tickets name GitHub PRs
  by URL, if at all, and following those would need the GitHub API inside a
  different adapter. `ticket.Prerequisites` is empty for them, so the envelope and
  the CI watch behave as before.
- Each candidate's prose references cost one issues read during selection, and
  each open PR one pulls read in `FetchTicket`.
- A ticket that names a PR left open indefinitely is never auto-selected. That is
  intended, and the skip line in the log says why.
