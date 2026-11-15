# ADR-0060: A green base unsettles a stale base-red finding and says so on it

- Status: Accepted
- Date: 2026-11-15
- Extends: ADR-0036 (the base-gates-red finding)

## Context

A red base files one `base-gates-red` finding, NeedsHuman, and its key has no
SHA in it so a `main` that stays red records recurrences on one issue
(ADR-0036). Once `main` goes green, that issue stays open until a human closes
it. Meanwhile it stays in the retrospective's settled list, since
`filing.Router.AlreadyFiled` is just the open tracker issues, and in the filing
dedup set. So a later, real red of the same class is deduped into the stale
issue instead of filing fresh, and nobody is told the old one can go (#311,
split out of #278).

## Decision

**A run whose implementation stage found its base green treats an open
`base-gates-red` finding as stale, and the run that first records a base green
says so on that finding, once.**

- **Unsettled on a green base.** The implementation stage reports the base SHA
  it found green, from a fresh gate or a recorded verdict
  (`stages.Result.GreenBase`). The pipeline hands it to the run's retrospective
  (`Args.GreenBase`), which leaves the `base-gates-red` key out of the settled
  classes and out of the dedup set (`filing.Router.Unsettled`). A
  base-gates-red finding the session reports then files a new issue: no exact
  match, no semantic match, no probable-duplicate flag against the stale one.
  A red verdict, a skipped check (a resumed branch, `--force`, a failed
  `post_create`) and the standalone `retrospective` command change nothing.
  So do the PR loop's and the security-alerts loop's retrospectives, which no
  base check precedes.
- **One comment per finding.** When a base's green verdict is first recorded,
  the stage looks up the open findings and posts `base gates green at <sha>` on
  each open `base-gates-red` one, so a human knows it may be closed. A mark per
  issue under `.agent-harness/locks/base-gates/told-green/` keeps a later green
  base from posting it again. A recorded green verdict read by a later ticket
  posts nothing: the run that recorded it already did.
- **The harness never closes it.** The finding is NeedsHuman and a human closes
  it, including when it is the ticket being worked. `tracker.FindingsSink` and
  the adapters gain no close or resolve method; the comment goes through the
  existing `Tracker.AddComment`.

## Alternatives considered

- **Close the finding on a green base.** A green base says the gates pass, not
  that the issue asked for nothing more (a regression test, a hardening); and
  the issue may be the ticket a session is working right now. Closing stays a
  human's call.
- **Read the verdict for the branch's base in the retrospective.** The
  retrospective runs after the push, by when the branch has been rebased onto a
  newer `main` that may never have been gated, so it would rarely fire. The
  implementation stage's own verdict is the one this run saw.
- **Comment on every green run, deduped by reading the issue's comments.** The
  tracker port lists no comments, and a search per dispatched ticket costs a
  tracker call on every green base. Once per green SHA, with a local mark per
  issue, costs one search per merge to `main`.

## Consequences

- While a stale `base-gates-red` issue is open, a green-base run's
  retrospective can file a second one for a new red. The new one carries the
  current failure, and the stale one already says the base went green.
- A red base after a green one still records a recurrence on the open issue,
  since the hold's own filing does not unsettle it. That issue then carries a
  "base gates green" comment followed by a recurrence, which reads in order.
- A comment that fails to post is not retried on a later green base: it was
  best-effort (ADR-0001). Deleting a mark under `told-green/` lets the next new
  green base post again.
