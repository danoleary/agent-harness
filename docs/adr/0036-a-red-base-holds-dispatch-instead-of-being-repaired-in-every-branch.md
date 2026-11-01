# ADR-0036: A red base holds dispatch instead of being repaired in every branch

- Status: Accepted
- Date: 2026-11-01
- Amends: ADR-0030 (adds a Disposition, BaseRed)

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
  (ADR-0024). A failed `post_create`, a resumed branch, `--force` and a ticket
  carrying the `base-gates-red` finding key all skip the check.
- **Verdict per base SHA.** The result is recorded under
  `.agent-harness/locks/base-gates/<sha>`, shared by the loop instances of one
  Consumer. A green verdict holds for the life of its SHA. A red one holds for
  30 minutes, since a red Gate can be a flake. A Gate that failed environmentally
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
- Two instances that reach an ungated base at the same time both gate it. The
  gate slots bound how many gate at once.
