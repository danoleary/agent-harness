# ADR-0067: A cap abort is read from the stream's rate-limit signal, and ends the slice

- Status: Accepted
- Date: 2026-11-22
- Extends: ADR-0030 (an interrupted slice skips the retrospective)

## Context

The harness recognised a spending-cap abort (BEH-494, BEH-568) by one message
string, `Spending cap reached`, in the `is_error` result or the
`model:"<synthetic>"` assistant turn. It read the reset time out of the same
prose (`resets 8:40am`, BEH-708) and assumed the harness's own timezone.

A newer CLI says "You've hit your session limit · resets 3am (UTC)" (#407). The
marker missed it, so three runs of one ticket against the account's five-hour
window fell through to other classes. The first was a generic "no handoff
commit". The next two ended on a `$0` result and were diagnosed as BEH-691's
deterministic prompt-expansion crash that needs a human. Each run then launched
a retrospective into the same window, which also died at once and wrote nothing.

The stream already says what happened in structured form, independent of the
wording: a `rate_limit_event` whose `rate_limit_info.status` is `rejected`, with
`resetsAt` as a Unix epoch, and a synthetic assistant turn with a top-level
`"error":"rate_limit"`. A healthy session near its limit carries
`allowed_warning`, which is not an abort.

## Decision

1. **The structured signals decide a cap abort.** `stream.IsSpendingCapAbort`
   matches a `rate_limit_event` with status `rejected`, or a synthetic assistant
   turn with error `rate_limit`. The text markers stay as the fallback, for both
   wordings: `Spending cap reached` and `hit your session limit`. No other
   rate-limit status counts.
2. **The reset is the event's epoch.** `stream.SpendingCapResetTime` returns
   `resetsAt` when the line carries it. The `resets h:mm am` regex is only the
   fallback, for a line that names no epoch. The session keeps the first cap
   line's reset, and the event comes before the prose.
3. **A cap-aborted implementation ends the slice.** The pipeline skips the
   retrospective, as it does for an interrupted slice (ADR-0030). The cap is
   still in force seconds later, so a retrospective launched into it dies the
   same way. The capped transcript stays under the ticket's logs, where the next
   run's retrospective reads it. A retrospective after a review still runs: a
   review that reached a verdict did not hit the cap.

The stages already check the cap abort before BEH-691's zero-real-turns branch,
so a `$0` result that follows a rejected event is the cap, not a prompt bug.

## Consequences

- A change to the CLI's wording no longer turns the retry-after-reset class
  into "needs a human". The loop backs off to the exact reset instead of
  re-dispatching.
- A cap abort costs one sandbox launch per run, not two.
- A slice whose implementation was capped mid-session has no retrospective of
  its own. Its friction reaches the next run's retrospective through the
  ticket's logs.
