# ADR-0063: The ticket loop leaves CI to the PR loop

- Status: Accepted
- Date: 2026-11-22
- Supersedes: ADR-0031 decisions 1 and 2 (`watch_ci` as an optional Step on a
  ticket loop). ADR-0031 decision 3, the branch hold as amended by ADR-0043,
  stands.

## Context

ADR-0031 made `watch_ci` an optional Step on a `tracker` loop. It kept the Step in
the built-in `tickets` loop, so a Consumer that runs no PR loop would still have
red CI on its ticket PRs fixed. A ticket loop that left the Step out ended its
review stage at the open PR (`config.Loop.LeavesCIToPRLoop`,
`stages.Args.LeaveCIToPRLoop`).

The watch costs what ADR-0031 measured: 9 to 16.5 minutes per ticket, 60–80% of
the review stage, with every sampled run going green without a fix session. The
retrospective, the worktree reap and the next ticket all wait for it. Meanwhile
the PR loop (`pr-doctor`) already fixes red CI on harness PRs once the ticket's
run is over, because the branch hold ends with the run (ADR-0043). Keeping the
in-stage watch meant keeping two CI fixers for ticket PRs, and, by default, the
slow one (#365).

## Decision

**`watch_ci` is no longer a Step of a ticket loop. The review stage's job ends
when the PR is open.**

1. The built-in `tickets` loop is `implement`, `review`, `retro`.
2. The review stage returns `Shipped` and OK as soon as the PR is open, and never
   calls `WatchCI`, on the daemon and on `pipeline` and `review` run by hand. Its
   closing line is `review ✓ PR open; CI is left to the PR loop`.
3. What only the in-stage watch used is deleted: the review stage's CIFix
   callback, the merged-prerequisite resolver (#82, ADR-0023) and
   `hostio.CIWatch.Resolve` behind it, the review's handling of the watch's
   zero-net-diff short-circuit (BEH-602), and `stages.Args.LeaveCIToPRLoop` and
   `config.Loop.LeavesCIToPRLoop`, since the case they named is now the only one.
4. A `[[loops]]` entry with a `tracker` Source that lists `watch_ci` still loads.
   The loader drops the Step and records it in `config.Loop.Ignored`. `doctor`
   warns on it (`ticket loop CI`), saying red CI on ticket PRs is fixed only by a
   running PR loop, and `loop --dry-run` prints it as ignored.

`watch_ci` on a PR loop (required when its Source yields red-CI PRs) and on the
security-alerts loop (optional) is unchanged, and so is `WatchCI`, the shared
BEH-602 short-circuit inside it, the branch hold, and the pre-push `Collapsed`
check (BEH-680).

## Alternatives considered

- **Keep the optional Step and keep it in the built-in `tickets` loop** (ADR-0031
  as it stood). Every Consumer that declares no `[[loops]]` pays the 10-15 minute
  wait per ticket for a watch that almost never fixes anything, and the harness
  keeps two CI fixers for one PR.
- **Keep the optional Step but drop it from the built-in loop.** The in-stage
  watch, its resolver and its config switch would stay for a Consumer who opts
  back in, which is a second CI fixer kept alive for one choice the PR loop
  already makes better: it fixes from a fresh checkout, and a conflict that is not
  a prerequisite's too.
- **A hard config-load error on a ticket loop's `watch_ci`.** It would stop every
  daemon whose config was written to ADR-0031 until a human edits it, for a Step
  that is now harmless. A warning in `doctor` says the same thing without the
  outage.
- **Detect a missing PR loop at runtime** and warn, or fall back to the in-stage
  watch. `doctor` cannot tell a declared loop from a running daemon, and
  `pr-doctor` is a daemon of its own: the ticket loop has no reliable way to know
  whether one is running, and a fallback would keep the code this removes.

## Consequences

- A Consumer that runs no PR loop has red CI on its ticket PRs wait for a human.
  The how-to, the example config and the release notes say to run
  `agent-harness loop pr-doctor` beside the ticket loop.
- A branch that becomes a no-op against `main` only after the pre-push rebase
  ships as `Shipped` instead of `RecommendClose`. Its PR stays open for a human to
  close.
- A PR that conflicts after a prerequisite merges is the PR loop's `Conflict`
  item, rebased by a RebaseFix session and re-gated, not an in-stage resolution.
- A red check is fixed later, from a fresh checkout, by the PR loop, not in the
  ticket's warm worktree.
- On the ticket loop, "Shipped but not OK" (CI red after the auto-fix budget) no
  longer occurs; the breaker never sees CI on a ticket run. It still occurs on the
  PR and security-alerts loops.
