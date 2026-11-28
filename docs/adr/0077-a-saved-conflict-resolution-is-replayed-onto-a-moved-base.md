# ADR-0077: A saved conflict resolution is replayed onto a moved base, not dropped

- Status: Accepted
- Date: 2026-11-28

## Context

Since #252 the PR loop saves a conflict resolution whose re-gate held back its
push, as `refs/harness/resolved/<slug>/<head>`. The next run at the same head
re-gates and pushes it with no session, but only while origin/main is still in
the saved tip. Once origin/main moved past that base, the saved ref was dropped
without a word in the run log, and a new conflict-resolution session resolved
the same conflict from nothing.

In #479 three pr-doctor runs started from one PR head. Each hit the same content
conflict in the same replayed commit, and each launched a session that worked
out the same hunk-by-hunk resolution again (about 19, 7 and 4 minutes of capped
session time). origin/main had moved between the runs, so each saved resolution
was discarded. One rerun put back a format-only defect that an earlier
resolution had already fixed.

## Decision

1. **A saved resolution on a stale base is kept.** `SavedResolution` reports it
   as stale and names the origin/main it was resolved onto. It is dropped only
   when a push lands, when a new resolution of the head replaces it, or when it
   is origin/main itself.
2. **The host replays it onto origin/main as it stands before any session.** A
   rebase cherry-picks the saved resolution's own commits onto origin/main, as
   Landing's replay does. A human's branch gets origin/main merged into the saved
   merge, so its history is not rewritten. The saved commits already settle the
   conflict, so only what origin/main gained since can stop git. A clean replay
   has a clashing ADR renumbered (rebase only, as #403 does) and is re-gated as a
   fresh resolution is. Green pushes it. Red saves it again for the next run, or
   holds the PR on a red base (#250).
3. **A replay git stops on goes to the session as a prior resolution.** The
   branch goes back to the PR head, the host's own replay finds the paths still
   unmerged (#403), and the RebaseFix session is told, before its job, the saved
   commit and its base. It is told how to read them (`git diff <base> <tip>`,
   `git show <tip>:<path>`) and to carry those hunks over rather than re-derive
   them.
4. **Every conflict run says what became of a saved resolution.** It logs one
   line: none was found; one was found and re-gated as it stands; one was found
   on a moved base and is being replayed, then whether that replay was clean or
   why it was set aside; or one was found but a recovery checkpoint took
   precedence.

`git rerere` is not used. Its cache lives in the checkout's shared git dir, so
turning it on means writing `rerere.enabled` into the operator's own repository
config. A host replay that rerere half-resolves leaves the paths unmerged, or,
with `rerere.autoUpdate`, staged. Either way the cherry-pick still stops, and the
replay classifies the stop as a conflict or a git failure. Replaying the saved
commits carries the same hunks without that state.

## Consequences

- A PR whose base moves between runs by commits that do not touch the resolved
  hunks ships its saved resolution with no session.
- A replay that conflicts costs one more host-side cherry-pick or merge before
  the session. That session starts from the earlier hunks, not from nothing.
- A stale ref lives until the PR is pushed or resolved again. Like any saved
  resolution, it is keyed on the head it resolved, so a new head never reuses it.
