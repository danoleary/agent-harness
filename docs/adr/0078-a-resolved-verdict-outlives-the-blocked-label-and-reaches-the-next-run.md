# ADR-0078: A resolved verdict outlives the blocked label and reaches the next run

- Status: Accepted
- Date: 2026-11-28
- Amends: ADR-0073 (a resolved-on-main ticket waits for close, not for main), decision 4

## Context

In #480 a run found its ticket already resolved on main and logged that the
ticket would not be dispatched again while main had the commit. About ten hours
later the loop dispatched it again into the same worktree, with a prompt
byte-identical to the first. The session re-derived the verdict from scratch,
named a different commit, and a retrospective ran again.

Two things let that happen. The ticket was a harness finding, and the
retrospective bumps a filed finding's `<!-- occurrences: N -->` marker in its
body each time it recurs. The no-work fingerprint hashed the body, so the
harness's own edit read as a human's, and the screen logged "the ticket changed
since" and dispatched it. And nothing told the new session what the last one had
concluded: ADR-0073 had Block delete the record, so after the blocked label landed
the verdict was gone, and a ticket dispatched once a human removed the label had
nothing to say about it either.

## Decision

1. **The fingerprint ignores the occurrence marker.** `nowork.Of` hashes the body
   with the marker and the blank line before it removed
   (`findings.WithoutOccurrences`). A body without the marker hashes as before, so
   existing records still match.
2. **Block marks a resolved record held instead of deleting it.** The record keeps
   its commit and gains the run's reason (`why`) and `held`. A plain no-work record
   is still cleared. The screen dispatches a held record's ticket whatever main
   and the fingerprint say: the adapter only offers it once the label is gone,
   which is a human's word that the commit does not fix it. This keeps ADR-0073's
   behaviour for the label and keeps the verdict.
3. **A dispatched ticket with a resolved record is told the verdict.** The
   implementation prompt names the commit and the reason, and asks the session to
   check that first: confirm it with `/findings/resolved.json`, or say what the
   ticket still needs and carry on. The run log says so. The run ends by
   rewriting or clearing the record as before, so it only ever describes the
   latest run.

The record stays keyed by the ticket. The resolving commit is the hold condition,
not the key, so a second session that names another commit overwrites the first.

## Alternatives considered

- **Ignore every label the harness writes in the fingerprint.** The ready,
  in-progress and blocked labels never differ at selection: a claimed or blocked
  ticket is not offered. That leaves only the body marker as a harness edit that
  reaches the fingerprint.
- **Keep the record unheld after Block.** Removing the label restores the labels
  the fingerprint was read with, so the record would hold the ticket for as long
  as main has the commit, and the label's removal would not dispatch it.

## Consequences

- A filed finding that recurs while it waits for close is no longer dispatched
  again by its own occurrence bump.
- A record written before this decision has no `why`; the next run's prompt names
  only the commit.
- A session told the prior verdict may lean towards repeating it. The prompt asks
  it to check the verdict against the ticket as it reads now and to carry on when
  the ticket asks for more.
