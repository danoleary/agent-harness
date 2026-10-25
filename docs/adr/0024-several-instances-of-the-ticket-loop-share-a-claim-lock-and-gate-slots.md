# ADR-0024: Several instances of the ticket loop share a claim lock and gate slots

- Status: Accepted
- Extends: ADR-0003 (claim on select), ADR-0019 (per-loop STOP and stream)
- Date: 2026-10-25

## Context

An operator wants two ticket loops against one Consumer, so that a deep backlog of
independent tickets drains about twice as fast (#89). It was unsafe for three
reasons:

1. **The claim was not exclusive.** ADR-0003 made selection a dequeue: select,
   then `MoveToInProgress`. Nothing confirmed that the run had won. Two loops that
   selected at the same moment both saw the top ticket unclaimed, both moved it In
   Progress (the second move is a no-op that succeeds), and both worked it on the
   same branch. No tracker the port covers offers a compare-and-set on a state
   change, and Jira's JQL search, which selection reads, is eventually consistent.
2. **Runtime state was per Consumer.** One `loop.pid`, one `STOP-tickets`, one
   `logs/loop.jsonl`. A second daemon could not be stopped or watched apart.
3. **Host load was unbounded.** Each run runs the Consumer's Gates in a container,
   and one Consumer already sees exit-137 OOM kills under one loop.

## Decision

**A ticket loop may run as several instances on one host. They claim under one
lock, confirm each claim with a fresh read of the ticket, keep their own runtime
files, and share a bounded set of gate slots.**

1. **Exclusive claim: a host lock around a confirm-then-claim.** `lease.Claim`
   takes `.agent-harness/locks/claim.lock` (a `flock`), calls the Tracker port's
   new `StillReady(key)`, and moves the ticket In Progress only if the ticket is
   still unclaimed. `StillReady` reads the one ticket, never the queue, so a lagging
   index cannot fool it: Linear reads the state type (`unstarted`), GitHub the
   labels (ready present, in-progress absent, open), and Jira whether the claim
   transition is offered and leads to a status other than the current one. Because
   the read and the move happen under one lock, no other instance's claim can land
   between them. A lost claim claims nothing, so it releases nothing, and
   selection runs again; when selection offers only tickets already lost this
   poll, the poll is empty. `pipeline --next` takes the same lock, so a single-shot
   run beside a loop is covered too.
2. **Instance-scoped runtime files, on #75's naming.** `--instance <id>` (on
   `start` and `loop`) names the instance. Its files carry the id:
   `loop-<id>.pid`, `loop-<id>.log`, `STOP-<id>`, `logs/instances/<id>/`. The
   default instance keeps every path it had. The sentinels nest: `STOP-<id>` stops
   one instance, `STOP-tickets` every instance of the ticket loop, `STOP` every
   loop. An id follows the loop-name rule and may not be a loop's name, so
   `STOP-<id>` is never also a loop's sentinel. Only a tracker-source loop runs as
   several instances: the PR loop's reaper assumes it is alone (ADR-0019 step b).
3. **Gate slots.** `GATE_CONCURRENCY` (default 1, `0` unbounded) slot files under
   `.agent-harness/locks/` (or `GATE_LOCK_DIR`) bound how many runs execute a gate
   set at once. `fix.Env.Gates`, the one entry point for every gate set (review,
   regates, the PR loop), holds a slot for the whole set. Only the gate step waits.
4. **Startup clear respects a running instance.** A starting daemon clears its own
   `STOP-<id>` always, and the shared sentinels only when no other daemon's
   pidfile names a live process; otherwise it would swallow a STOP another
   instance has not yet reached a checkpoint to see.
5. **Branch uniqueness follows from the claim.** The branch is
   `<branch_prefix>/<slug(Key)>`, so two instances that never hold one Key never
   produce one branch. A test drives four instances through one lagging queue and
   checks no branch is produced twice.
6. **The reaper needs no change.** It releases only a claim past the TTL with no
   pushed branch and no linked PR. A working instance's claim has one or the other
   by then, and a test runs one instance's reaper while another works a ticket.

## Alternatives considered

- **Assign the ticket to a per-instance identity and check the assignee after the
  move**, as the ticket suggested. It would also cover instances on different
  hosts. But every instance acts as the same tracker user or bot, so each instance
  would need a tracker account of its own, and Jira and Linear assignments need
  user ids the Consumer config does not carry. Rejected for now. The
  `StillReady` read is where such a check would plug in.
- **A claim marker comment, with the earliest one winning.** It needs a comment
  read on every adapter, and every tracker's comment ordering and pagination, for a
  protocol a lock on one host already makes unnecessary. Rejected.
- **Only the lock, with no confirmation read.** Under the lock, Linear's and
  GitHub's list reads would already see the first claim. Jira's search can lag it
  by seconds, and the second instance would claim the ticket again. Rejected.
- **A second clone per instance with split ready labels** (the old workaround). It
  costs a manual split of the backlog and bounds nothing on the host. Rejected.

## Consequences

- Every Tracker adapter implements `StillReady`; a fake that does not would not
  satisfy `tracker.Queue`.
- Instances must share a host: the claim lock and the gate slots are files. Two
  hosts against one queue are not covered, and the docs say so.
- A claim takes one extra tracker read per selection (two on Jira).
- A lost race costs the losing instance at most one poll interval when the queue's
  index lags; with a consistent queue it selects the next ticket at once.
- `.agent-harness/locks/`, `STOP-*`, `loop-*.pid` and `loop-*.log` are runtime
  files; `agent-harness init` adds them to `.gitignore`.
