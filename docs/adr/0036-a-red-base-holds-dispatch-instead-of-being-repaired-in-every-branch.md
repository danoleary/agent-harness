# ADR-0036: A red base holds dispatch instead of being repaired in every branch

- Status: Accepted
- Date: 2026-11-01
- Amends: ADR-0030 (adds a Disposition, BaseRed)
- Amended: 2026-11-14 (#278: a base-repair ticket is gated, not held)

## Context

Two concurrently merged PRs left `main` with a test file that no longer
compiled. Every implementation session cut from that base found the red Gate
mid-session, diagnosed it from scratch, and repaired it in its own feature branch
as an out-of-scope change. Two sessions running at once made slightly different
edits to the same lines. Whichever PR landed second conflicted with base, so its
CI watch failed and the ticket stalled with its PR and worktree, even though its
own change was clean and had passed review and the host gate (#125).

So a red base cost a diagnosis per session, unrelated churn in every PR diff,
and a merge conflict for every sibling branch after the first one landed. That
is separate from keeping `main` green in the first place. This decision is about
what the stage flow does once it is red.

## Decision

**The implementation stage gates the untouched base before launching a session,
and a red base holds the ticket without launching one.**

- **When.** After the worktree is created and `post_create` succeeds, if the
  branch carries no commits and no uncommitted work, the stage runs the
  configured Gates over it (`basegate-<name>`), under the gate slots
  (ADR-0024). A failed `post_create`, a resumed branch and `--force` skip the
  check.
- **The base is current `main`** (#334). A reused worktree whose branch has no
  commits ahead and a clean tree holds nothing to preserve, so before
  `post_create` the stage fetches `origin/main` and resets the worktree onto
  it. Left on the base it was cut from, the session would read superseded code
  and the verdict would be recorded for a SHA `main` had already left behind. A
  branch with commits or uncommitted work stays where it is.
- **A base-repair ticket is gated but never held** (#278). A ticket carrying
  the `base-gates-red` finding key is the change that turns `main` green, so a
  red base dispatches it as before and files nothing more. But `main` may have
  gone green while the ticket waited in the queue. Its sessions then spent their
  first turns finding that out by hand. So on a green base the session still
  launches, and its envelope says the base gates are green at that SHA, so it
  adds only the regression test or hardening the ticket asks for. Skipping the
  session and reporting the ticket resolved was the alternative; a human would
  still have to confirm it, and the ticket may ask for more than the repair.
- **Verdict per base SHA.** The result is recorded under
  `.agent-harness/locks/base-gates/<sha>`, shared by the loop instances of one
  Consumer. A red run is confirmed before it is recorded: the Gates run once
  more over the same base (`basegate-confirm-<name>`), and the base is red only
  if that re-run is red too. A pass on the re-run records the base green and
  the ticket dispatches (#189). A green verdict needs no confirming, since a
  flake can only make a base look red, and holds for the life of its SHA. A red
  one holds for 30 minutes, since a Gate can flake on both runs. A Gate that failed environmentally
  (an OOM-kill or failed launch through its retries, or its cap) records nothing,
  and the ticket dispatches.
- **Hold.** On a red base the stage discards the untouched worktree and branch,
  so the next attempt is cut from wherever `main` has moved. It releases the
  claim and returns a new Disposition, **BaseRed**. The pipeline ends the slice
  there, with no review and no retrospective, since no session ran. The loop
  refetches `origin/main` and backs off one poll interval. The breaker ignores
  BaseRed: the red is `main`'s, shared by every ticket on it.
- **One finding.** The run that first records a red verdict files a
  `base-gates-red` finding with `needs_human` set. It names the SHA, the Gate and
  its stderr tail. Its key has no SHA in it, so a `main` that moves but stays red
  records a recurrence on the open issue rather than filing another.

## Alternatives considered

- **File an agent-ready repair ticket and prioritise it.** The tracker port has
  no priority it can set, and the loop re-selects the held tickets, which sit
  ahead of a newly filed issue in the queue. The repair would starve behind the
  hold. A human repairs `main` or dispatches the issue by hand. The finding key
  exempts it from the hold.
- **An envelope rule only: stop and report a Gate that also fails on base.**
  Every session would still be launched and still re-diagnose the same red Gate
  before stopping. The host can answer the question once per SHA.
- **Gate the base at selection, before the claim.** Gates need a provisioned
  worktree, which is what the implementation stage builds. Gating there reuses
  `post_create` and the worktree the session would run in.
- **Rebase automatically when the CI watch finds a conflict with base.** That
  helps the sibling that lands second, but only after each session has already
  repaired the base. It is still worth doing as its own change.

## Consequences

- Each new base SHA costs one extra Gate run before its first ticket. While
  `main` stays green that is roughly one per merge; while it is red, none after
  the first.
- While `main` is red the loop dispatches nothing. That is the point, but it
  depends on a human seeing the finding.
- A red base costs two Gate runs before its hold, so a single flaky Gate step
  does not hold every ticket on a green `main` for 30 minutes and file a
  finding for it (#189). A re-run that fails environmentally leaves the red
  unconfirmed, records nothing, and the ticket dispatches.
- Two instances that reach an ungated base at the same time both gate it. The
  gate slots bound how many gate at once.
