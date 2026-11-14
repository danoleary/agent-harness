# ADR-0054: A commit that landed on a file the ticket names is named at dispatch

- Status: Accepted
- Date: 2026-11-14
- Extends: ADR-0010, ADR-0028

## Context

#317 asked for the entrypoint-trust tests to resolve their temp workdir, so they
would pass on macOS, where `/var` is a symlink to `/private/var`. Commit aa78a20
(#318) made that change to `internal/sandbox/entrypoint_trust_test.go`, the file
#317 names. Its subject named the PR (#318) and not the ticket, so the ticket was
never closed. The own-key dispatch guard (`TicketAlreadyOnMain`, BEH-528) greps
recent `origin/main` subjects for the Key and found no `#317`. #317 was dispatched
six times. Each session spent its turns finding the fix on main by hand, made no
commit, and failed with "no handoff commit" (#335).

## Decision

**Every tracker adapter reports when a ticket was filed. At dispatch, the
implementation stage lists the commits that landed on `origin/main` since then
and changed a file the ticket names. It warns in the run log and names those
commits in the envelope. It never skips the ticket.**

- **Filed time (`ticket.Ticket.Created`).** GitHub reads `created_at`, Linear
  `createdAt`, Jira `fields.created`. Zero when the tracker gives none. Then the
  check is off for that ticket.
- **Scan (`git.Checkout.LandedSince`).** `git log --since=<filed> --name-only`
  over the origin/main that `TicketAlreadyOnMain` just fetched, within the same
  50-commit lookback. It uses committer dates, because a squash or merge commit's
  committer date is when it landed. Any git error lists nothing.
- **Match.** A ticket names files as the overlap check reads them (`overlap.Named`,
  ADR-0032, ADR-0047), so a directory, a generic name or a fenced block names
  none. The advisory lists up to five commits, newest first.
- **Advisory and envelope.** The run log line is `⚠ <KEY> names files that
  changed on main since it was filed: …`. Every Implement variant names each
  commit and the files it changed, and tells the session to read them before
  writing code. If one already does what the ticket asks, the session makes no
  commit, writes the `/findings/resolved.json` marker naming that commit (so the
  run is ResolvedOnMain, ADR-0052), says "already fixed on main by <sha>" in its
  handoff, and stops.

## Alternatives considered

- **Skip the ticket, like `TicketAlreadyOnMain`.** A commit that changed a file
  the ticket names is weak evidence. Busy files change all the time, and a
  ticket often names a file it wants changed again. A false skip drops real work
  and leaves no sign of it. A session that checks one named commit first costs
  little.
- **Match the ticket's symptoms or test names against commit messages.** Prose
  matching is noisy, and the commit message may not mention the symptom at all.
  Files are what a fix has to touch.
- **Flag a ticket whose number is next to a just-merged PR's.** That only works on
  GitHub, where issues and PRs share one sequence of numbers. Two numbers being
  adjacent says nothing about whether one fixes the other.

## Consequences

- A fix to a file the ticket does not name, or one that landed more than 50
  commits back, is not detected. That is the same reach as the own-key scan.
- A ticket that names a busy file may get the advisory for unrelated commits. The
  session reads them and carries on, at the cost of a few turns.
- A session that stops this way still ends with "no handoff commit". Recording
  that stop as its own outcome is a separate change (#329).
