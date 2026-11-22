# ADR-0061: A run that cannot take its run lock runs nothing

- Status: Accepted
- Date: 2026-11-22
- Amends: ADR-0043 (decision 1, "the lock is only a signal")

## Context

ADR-0043 made the run lock (`.agent-harness/locks/runs/<slug>.lock`) the sign
that a run of a ticket is live, so the PR loop stays off its branch. It was only
a signal: a run that found the lock taken logged `another live run holds <key>'s
run lock; running without it` and went ahead.

In herd (BEH-1470, BEH-1483) that let two and once three runs of one ticket
work its worktree at the same time (#366). Each time the first run's sandbox
started, the host slept 9 to 11 hours, and the sandbox resumed on wake. The
stale-claim reaper reads a claim's age by the wall clock, so the live run's
claim was far past `LOOP_CLAIM_TTL_MS` with nothing pushed yet. The reaper
released it to Todo, the next poll selected it, and the new run took no lock and
started its Stages anyway. Three things went wrong:

- the newer run's sessions saw files change under them and stopped, wasting a
  full sandbox cap;
- its recovery checkpoint committed the other run's half-finished edits as its
  own, so a commit that did not type-check landed under the wrong attribution;
- one run pushed and opened the PR from a tree a sibling's conflict resolution
  had just rebased, so the push went out on a tree its gates never saw.

## Decision

**Every run of a ticket holds its run lock, and a run that cannot take it runs
nothing. The selection, the reaper and the recovery checkpoint all read the
lock, not the clock.**

1. **The lock is a gate.** `loophost.HoldRun` returns `ErrRunLive` when another
   live run holds the lock, and an error when the lock cannot be taken at all.
   It narrates either and the run launches no session. This covers the loop's
   Pipeline and its committed-fix recovery, a single-shot `pipeline`, and the
   standalone `implementation`, `review` and `retrospective` commands. The
   `running without it` branch is gone.
2. **RunLive.** In the loop, a run or recovery refused for a live run comes back
   with the new Disposition **RunLive**. The loop leaves the claim alone, tries
   no recovery, counts no attempt toward `LOOP_MAX_TICKETS`, leaves the breaker
   alone and selects the next ticket without a backoff. The ticket asked that
   the refused run release its claim. It does not, because the claim belongs to
   the live run: releasing it would put that run's ticket back in Todo, and
   once the run ended the next poll would start the ticket over. A lock that
   cannot be taken is a plain no-PR run, so a broken locks directory trips the
   breaker as any environmental failure does.
3. **Selection passes over a live ticket.** `pipeline.SkipLive` is the first
   screen in `TicketSource.Resolver`, which `pipeline --next` shares. A ready
   ticket whose run lock is held is skipped with `skipping <key>: another live
   run holds its run lock`. A lock that cannot be read is skipped too: a wrong
   skip costs one poll, but a wrong dispatch puts two runs on one worktree.
4. **The reaper spares a live run.** `loop.StaleClaim.Live` is true when the
   claim's run lock is held, or cannot be read. The reaper never releases such a
   claim, however old it reads. The claim TTL still decides for a claim with no
   live run.
5. **The checkpoint is the lock holder's.** `HoldRun` records the run id in the
   lock file (`hostlock.File.TryLockAs`). Before a Stage commits a recovery
   checkpoint, it reads the holder (`hostlock.File.Holder`). If another run
   holds the lock, or the lock cannot be read, it leaves the edits uncommitted
   and narrates why.

## Alternatives considered

- **Time the claim TTL on a monotonic clock, or subtract the host's sleep.** A
  claim's age comes from the tracker, which knows nothing of this host's sleeps,
  and another instance or host reads the same claim. The lock is a fact about
  the run and needs no clock.
- **Release the claim on a refused run, as the ticket proposed.** See decision
  2: the claim is the live run's.
- **Back off one poll after a refused run.** Selection already passes over the
  ticket, so a backoff only idles the other ready tickets.
- **Leave the checkpoint unguarded, since a run now always holds its lock.** A
  Stage run by an older binary, or by a process that took no lock, can still
  share the worktree. The guard costs one probe per failed session.

## Consequences

- ADR-0043's "the lock is only a signal: if another run already holds it, or it
  cannot be taken, the run says so and goes ahead" no longer holds.
- A run started by a binary from before this ADR records no holder name. A new
  binary reads that lock as held by an unnamed run, and refuses to start or
  checkpoint alongside it.
- Running a Stage by hand while the loop works the same ticket now exits 1
  without launching anything.
- The security-alerts and PR loops are unchanged: their items are not tickets,
  and the PR loop already stays off a held branch (ADR-0043).
