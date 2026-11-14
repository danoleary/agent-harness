# ADR-0052: A session's already-resolved-on-main stop is handed to a human, not failed

- Status: Accepted
- Date: 2026-11-14
- Amends: ADR-0030 and ADR-0042 (adds a Disposition, ResolvedOnMain)

## Context

The `/tdd` skill tells an implementation session that finds its ticket already
fixed on `main` to say so and stop: "that is a legitimate outcome". The host had
no way to see that outcome. In #329 a commit for a different ticket, one that
never named this ticket, had already fixed it on `origin/main`. The own-key skip
(`TicketAlreadyOnMain`) could not match that commit. The symbol advisory
(`ResolvedAdvisory`) stayed quiet because the fix added a helper and deleted
nothing. The session spotted the fix in a few turns and stopped without
committing. `verify.Tdd` counts only commits ahead of the base, so the stage
logged `tdd ✗ no handoff commit ahead of origin/main`. The loop released the
ticket as a no-PR failure, the breaker counted it, and the session's reasoning
was only in the transcript.

## Decision

**A fresh implementation session reports the stop in a marker. The host checks
the marker against git and records the stop as its own Disposition,
`ResolvedOnMain`.**

- **The marker.** The Fresh envelope tells the session to stop without
  committing and write `/findings/resolved.json` as `{commit, reason}`, naming
  the `origin/main` commit that already fixes the ticket. The host clears any
  marker an earlier run left before each session, because the findings dir is
  keyed on the ticket, not the run. The resumed-branch envelope does not offer
  the marker: a branch with commits ahead is a handoff, not this stop.
- **The ground truth.** The host reads the marker only when the session left no
  handoff: the worktree is clean and nothing is ahead of `origin/main`. It
  accepts the marker only if the named commit resolves and is an ancestor of
  `origin/main` (`Worktree.OnBase`). A marker that does not hold leaves the run
  the failure it was, and its reason names the marker's commit.
- **The Disposition.** A marker that holds gives `ResolvedOnMain`, with the
  full SHA in `Result.ResolvedBy` and the session's reason in
  `Result.ResolvedWhy`. The stage logs `tdd ≡ already resolved on main by
  <sha> — recommend close: <reason>`. It is not OK, since nothing was handed off,
  so the pipeline skips review. But the slice did its job, so the pipeline and
  the standalone `implementation` exit 0.
- **The loop.** The loop does not release the ticket, which would re-run it to
  the same stop. It does not close it either, as it closes a RecommendClose, since
  that would act on the session's judgement of whether the commit really fixes
  the ticket. It keeps the claim, labels the ticket blocked (as for NeedsHuman,
  ADR-0042), and comments the commit and the reason, asking a human to confirm and
  close it. The breaker treats it as neutral.

## Consequences

- A sanctioned stop no longer counts toward the breaker or reads as a failure in
  the run log, and the ticket leaves the queue with the evidence on it.
- The pre-dispatch own-key skip and the symbol advisory stay as they are. This
  catches what they miss only after a session has been paid for. That costs a few
  turns, where the old way cost a failed run and a human digging through a
  transcript.
- Trust stays with git. A session can name a wrong commit that is on main. The
  ancestor check cannot catch that, and that is why the ticket goes to a human
  rather than being closed.
