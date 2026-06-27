# ADR-0003: `--next` claims the ticket during selection, before Docker preflight

- Status: Accepted
- Date: 2026-06-27

## Context

`pipeline --next` selects the top-of-queue eligible ticket instead of taking it as
an argument (see DESIGN.md "Single-shot auto-select"). Unlike a hand-passed ticket
— where two humans rarely name the *same* ID — `--next` is **deterministic**: two
selections (a stray concurrent invocation, or the future loop's next turn) both
compute the *same* top-ranked ticket. If selection is a pure read, both then claim
it and both try to create worktree `beh-nnn` at the same path — a race that
manifests routinely rather than theoretically.

The implementation stage deliberately claims a ticket (`MoveToInProgress`, Todo →
In Progress) **late** — *after* Docker preflight — with the standing rationale
"fail fast if Docker can't run the container, so we never claim a ticket we cannot
actually work" (the BEH-316 footgun: a launch failure leaving a ticket stranded In
Progress with no worktree). That ordering is load-bearing for the hand-passed path.

So `--next` forces a choice between two goods that pull apart: **a narrow claim
race** (claim early, to dequeue) versus **never claiming a ticket we can't work**
(claim late, after preflight).

## Decision

`selectNextTicket()` **claims the ticket as part of selection**, before returning —
giving selection *dequeue* semantics. Once claimed, the ticket leaves the eligible
set (it is no longer Todo), so a concurrent or subsequent selection skips it and
takes the next one. The race window shrinks from "everything up to the stage's
preflight" to a single GraphQL round-trip between the eligibility query and the
claim mutation.

This means claiming now happens **before** Docker preflight on the `--next` path,
inverting the BEH-316 ordering. We pay that back with two mechanisms, both threaded
by a `PreClaimed` flag set during selection:

- **Release-on-preflight-failure.** When the implementation stage's preflight fails
  *and* `PreClaimed` is set, the stage calls `ReleaseToTodo`, returning the
  dequeued-but-unworkable ticket to the queue. The end state — ticket back in Todo,
  no worktree — is identical to BEH-316's; only the path differs (claim-then-release
  rather than never-claim), with a transient In-Progress flicker in between.
- **`PreClaimed` leaves the hand-passed path untouched.** On `pipeline BEH-NNN`,
  `PreClaimed` is false: preflight still precedes the claim exactly as today, there
  is nothing to release, and the BEH-316 ordering is unchanged. The new complexity
  lives entirely on the new path.

## Alternatives considered

- **Keep selection a pure read; claim stays late; declare concurrency out of scope.**
  Relies on the harness's "sequential only (v1)" invariant and lets a stray second
  `--next` collide harmlessly at worktree creation (the second run fails cleanly, no
  corruption, no PR). Simpler, and it preserves BEH-316 verbatim — but it leaves a
  wide race window precisely where `--next` makes the same-ticket collision *likely*,
  and it gives the future loop nothing to build dequeue semantics on. Rejected
  because the loop will need a real dequeue, and building it now (while a human is
  still at the trigger to catch surprises) is cheaper than retrofitting it later.
- **Unify both paths: always claim-before-preflight + always release-on-failure.**
  One invariant for every caller, no `PreClaimed` branch. Rejected as scope creep: it
  changes the tested hand-passed behavior (introducing an In-Progress flicker where
  today there is none) on a path BEH-316 hardened, spending regression risk on code
  the `--next` work was never asked to touch.

## Consequences

- Concurrent or looped selection cannot double-grab the top ticket; the race is one
  GraphQL round-trip wide instead of one whole sandbox launch wide.
- The future loop reuses `selectNextTicket()`'s claim-on-select directly — dequeue is
  built once, here.
- A `--next` ticket that fails Docker preflight flickers In Progress → Todo rather
  than never leaving Todo. The observable end state is unchanged; a watcher tailing
  Linear may briefly see the claim.
- Claiming now lives in two places (selection for `--next`, the stage for hand-passed)
  reconciled by `PreClaimed`. This is the deliberate price of leaving the hardened
  hand-passed path untouched; a single unified claim site was the rejected
  alternative.
- `release-on-preflight-failure` is the second sanctioned exception to "failure never
  mutates Linear state" (the first being BEH-543's no-worktree release) — both safe
  for the same reason: there is no partial work to protect.
