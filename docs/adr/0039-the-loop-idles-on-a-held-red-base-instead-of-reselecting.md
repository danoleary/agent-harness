# ADR-0039: The loop idles on a held red base instead of re-selecting

- Status: Accepted
- Date: 2026-11-07
- Amends: ADR-0036

## Context

ADR-0036 holds a ticket whose base fails a Gate. After a BaseRed run, the loop
waited one poll interval and selected again. The cached verdict is read inside the
implementation stage, after selection, the claim, worktree creation and
`post_create`. So while `main` stayed red, every poll repeated that whole cycle
for a hold the recorded verdict had already decided. In the incident behind #190,
main was held red for 30 minutes with a one-minute poll: about 26 cycles, each with
a tracker claim and release, a `post_create` run, and a block in the run log that
buried the one real gating event.

## Decision

**After a BaseRed run, the loop remembers the held base and when its red verdict
expires, and idles until one of them changes.**

- **The record.** A BaseRed Result carries the base SHA it held on
  (`HeldBase`) and the recorded verdict's expiry (`HeldUntil`: the verdict's
  time plus the 30-minute red recheck). A hold read from an existing verdict
  expires with that verdict, not 30 minutes after the read.
- **Each later poll** fetches `origin/main` and reads its SHA in the primary
  checkout before selecting. If main is still on the held base and the verdict
  has not expired, the loop writes one `still held on <sha>, re-check at HH:MMZ`
  line and idles a poll interval. It makes no tracker call and touches no worktree.
- **The hold ends** when main moves, when the verdict expires, or when main's
  SHA can't be read. The loop says which, forgets the hold and selects as before.
  A hold it can't confirm must not idle the loop.
- Stop checks, the runtime and ticket ceilings, disk reclaim and the stale-claim
  reaper still run on every poll. The breaker still ignores BaseRed.

## Alternatives considered

- **Read the verdict record at selection.** The loop would need the verdict
  store, which belongs to the Stages. The SHA and expiry on the Result are all
  the loop needs.
- **Back off for the whole verdict lifetime.** That waits up to 30 minutes after
  a repair merges. Comparing main's SHA on each poll ends the hold on the first
  poll after the merge.

## Consequences

- While main is red, each poll costs one fetch and one `rev-parse` instead of a
  claim, a worktree, a `post_create` run and a release.
- While the hold lasts, the loop selects nothing at all. That includes the
  `base-gates-red` repair ticket and a resumed branch, both of which ADR-0036
  exempts from the hold. Either waits until main moves or the verdict expires,
  at most 30 minutes. A human can still dispatch the repair by hand.
- Each loop instance keeps its own record, so another instance on the same
  Consumer runs one full cycle before it holds.
- The record is in memory, so deleting a verdict under
  `.agent-harness/locks/base-gates/` no longer re-gates the base on a running
  loop's next poll: that loop idles until the re-check time unless it is
  restarted.
