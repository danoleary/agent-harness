# ADR-0048: A red re-gate over a red base holds the PR instead of blaming it

- Status: Accepted
- Date: 2026-11-10
- Amends: ADR-0036, ADR-0039 (extends the red-base hold to the PR loop)

## Context

A PR loop conflict resolution rebased a branch onto origin/main and resolved it
cleanly. But origin/main did not build by itself: two PRs had merged without
seeing each other's change, so an interface gained a method that one
implementation lacked. The session said so. The host's `make check` re-gate
then went red on main's compile error, and `fix.ResolveConflict` reported a
failed resolution as if the branch were at fault (#250).

The resolution was thrown away. Every later poll redid the rebase, spent
another session and went red again until main was fixed, using up the PR's
attempt budget. ADR-0036 handles a red base for the ticket loop: it gates the
untouched base, records the verdict per SHA and holds the work. The conflict
path had nothing like that.

## Decision

**When the re-gate over a resolved branch is red, the harness checks whether
origin/main fails that same Gate on its own. If it does, the red is the base's,
and the PR is held, not blamed.**

- **Shared record.** The per-SHA verdict record, its 30-minute red expiry and
  the `base-gates-red` finding moved from `internal/stages` to
  `internal/basegate`. The implementation stage and `internal/fix` both use it,
  so a base either one gated is gated once for both.
- **The check.** `fix.ResolveConflict` runs it only for a real red, not an
  environmental one. A verdict already recorded for origin/main's SHA answers
  it. Otherwise origin/main is checked out alone in a throwaway detached
  worktree (`git.Worktree.TrialBase`, at `<worktree>.base`), provisioned, and
  gated under the same `basegate`/`basegate-confirm` labels. As in ADR-0036, a
  red counts only if the re-run is red too, and an environmental red records
  nothing.
- **Same Gate only.** The base is to blame only if it fails the Gate the
  re-gate failed. A green base, or a base red on a different Gate, leaves the
  red with the branch, as before.
- **Outcome.** `ConflictResult.BaseRed` names the base SHA and its verdict. The
  log says `base is red, not the branch`. The PR loop ends the run as a
  **BaseRed** Disposition carrying `HeldBase` and `HeldUntil`, so the daemon
  idles on that base exactly as ADR-0039 describes. Its Source voids the
  attempt it recorded (`HoldOnBase`) and leaves the PR unlabelled. The run that
  recorded the red verdict files the `base-gates-red` finding into the
  tracker's findings team; a run that only read the verdict files nothing.
- The review stage's pre-push resolution gets the same check. Its breadcrumb
  names the red base instead of blaming the branch.

## Alternatives considered

- **Trust the session's report that main is red.** The host never trusts a
  session's say-so for a push decision (ADR-0016). A structured "base does not
  build" report could still save the session, and the gate run, before the
  host checks. It is left for later.
- **Gate the base before every conflict session.** That spends a gate run on
  every conflict. Checking only after a red re-gate costs nothing while main is
  green.

## Consequences

- A red re-gate on a base the record has not yet seen costs a `post_create`
  and up to two more Gate runs.
- While the hold lasts, the PR loop selects nothing, including CI-red PRs whose
  CI merges with the same red main. The hold ends when main moves or the
  verdict expires (ADR-0039).
- The resolved branch is still not pushed. The run after main moves redoes the
  resolution from scratch.
