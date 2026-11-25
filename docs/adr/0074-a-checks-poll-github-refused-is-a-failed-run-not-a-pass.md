# ADR-0074: A checks poll GitHub refused is a failed run, not a pass

- Status: Accepted
- Date: 2026-11-25
- Extends: ADR-0069 (CI still pending past the poll budget is unknown, not red)

## Context

The CI watch polls a PR's checks with `gh pr checks`. When GitHub refused that
poll's credentials, with an HTTP 401 / bad credentials (BEH-627) or a 403
"resource not accessible" from a token that cannot read check runs (BEH-476),
the watch failed soft to a green pass. The reasoning came from the ticket
loop's old post-push watch: the PR had just been pushed gate-green, so an
unreadable CI was only something for a human to eyeball.

The PR loop does not open on a freshly pushed, gate-green PR. It opens on a PR
whose CI is already red. In #466 its first checks poll got a 401. The run logged
`pr-fix ✓ CI watch unavailable`, counted as Shipped, removed the worktree and
launched no fix session. The PR stayed red until a second run about ten minutes
later authenticated, re-ran the failed checks and fixed it. A credential fault on
the host looked like a finished run, and the red PR waited for a later tick.

## Decision

1. **A refused poll is polled once more.** gh reads its credentials afresh on
   every call, and the refusal in #466 had cleared within minutes, so the driver
   retries a 401 or a 403 once, one poll interval later, and narrates the retry.
   A retry that reads checks goes on as a normal watch.
2. **A refusal that survives the retry is its own outcome.**
   `ci.Outcome.AuthFailed` marks it, not OK, apart from a red CI, a poll fault
   and a pending CI. The watch read no CI state, so it runs no re-run and no fix.
   This holds for both loops that watch CI.
3. **The PR and security-alerts loops narrate it as a failure.** The line is
   `✗ ci-watch-auth-failed: CI watch read no CI state: …`, never a `✓` and never
   "CI did not go green".
4. **A PR run that pushed nothing before the refusal is CIAuthFailed.** It is a
   new **Disposition**. The Source takes the attempt back with a void marker and
   a comment, as for CIPending, and the PR stays unlabelled. The worktree is
   kept, not removed: the run ended without a CI verdict. The loop releases the
   claim and waits one poll before it selects again, so the PR is worked on the
   next tick. Unlike CIPending, the breaker counts it: a credential fault fails
   every PR, and the breaker is what stops a loop on an expired token. A run
   that pushed a fix before the refusal stays **Shipped**.

## Consequences

- A red PR whose checks poll was refused is no longer reported as done, and it
  is worked again on the next tick, not whenever its next attempt comes round.
- A token that cannot read check runs (a fine-grained PAT) now fails every PR
  run instead of passing it, and trips the breaker. `agent-harness doctor`
  already requires a classic `repo`-scoped token.
- A kept worktree is still replaced at the next checkout, which removes a clean
  worktree and adds it again at origin's tip.
- The `Disposition` set grows by one. The ticket loop never produces it.
