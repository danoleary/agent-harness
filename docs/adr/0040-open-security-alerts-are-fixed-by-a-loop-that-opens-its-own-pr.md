# ADR-0040: Open security alerts are fixed by a loop that opens its own PR

- Status: Accepted
- Date: 2026-11-07
- Extends: ADR-0019 (a third Source and Pipeline)
- Amends: ADR-0002 (the sandbox's network reach, reworded)

## Context

A Consumer's open Dependabot alerts are work the harness could do, but nothing
in it reads them. ADR-0019 built two loops. `tickets` works the tracker's ready
queue and lands each ticket as a new PR. `pr-doctor` works open PRs that conflict
or have red CI and pushes to the existing branch. A security alert fits neither:
it has no ticket and no PR.

Dependabot opens its own PRs, but on herd most alerts are in pnpm transitive
dependencies. Dependabot often cannot move a transitive dependency to a patched
version, and it never writes the `overrides` entry in `pnpm-workspace.yaml` that
pins one. Those alerts stay open until a human writes the override.

The fix is small and mechanical: bump a direct dependency, or add an override,
then regenerate the lockfile. Whether it worked can be checked on the host
without trusting the session. The lockfile either still resolves a vulnerable
version or it does not.

## Decision

**A third loop instance, the `github_security_alerts` Source, fixes open
Dependabot alerts in a fresh branch and lands each fix as a new PR.** No tracker
ticket is created.

```toml
[[loops]]
name = "security-alerts"
source = { kind = "github_security_alerts", severities = ["critical", "high"] }
steps = [
  { use = "fix", prompt = "prompts/security-fix.md" },
  { use = "gates" },
  { use = "ship" },
  { use = "watch_ci" },
  { use = "retro" },
]
```

1. **The Source yields one Work item per vulnerable package.** It reads the open
   Dependabot alerts at the configured severities on the host and groups them by
   package. One Work item covers every open alert on that package, so one PR
   fixes them together. Its branch is cut on the harness's `branch_prefix`.
2. **An alert with no patch is skipped without a session.** If no patched version
   exists for an alert, no dependency change can fix it. The Source logs the skip
   and yields nothing for it. A package with some unpatched alerts is worked for
   the patched ones.
3. **The Pipeline cuts a fresh branch from `origin/main`** and runs one sandboxed
   SecurityFix session in its worktree. The session gets the SecurityFix
   envelope (ADR-0009) with the alerts' package, vulnerable ranges and patched
   versions, plus the Consumer's body. It commits locally, as every session does.
4. **The host checks the result before any Gate runs.** The session's
   self-report is not trusted (ADR-0016). The branch must have a committed diff,
   and the diff may touch only dependency files: manifests, lockfiles, and the
   workspace file that holds overrides. The lockfile must no longer resolve any
   version of the package inside any of the item's alerts' vulnerable ranges. A
   result that fails either check pushes nothing.
5. **Then the shared Steps run:** the Gates, Landing with `ship` as a new PR
   (ADR-0017), the CI watch, and the retrospective, as in the ticket pipeline.
6. **Attempts are recorded where they can be found again.** A pushed attempt
   writes a hidden marker in the PR body. An attempt that pushed nothing writes a
   line to a ledger in the loop's logs (`logs/loops/<name>/`), because there is no
   PR to write to.
7. **A fingerprint over the alert set limits retries.** The marker and the ledger
   line each carry a fingerprint of the item's alerts: their numbers and
   vulnerable ranges. A package with an open PR whose marker matches is skipped. A
   package whose ledger already holds that fingerprint `max_attempts` times is
   skipped. A new alert, or a changed range, changes the fingerprint and starts a
   fresh budget.
8. **The lease is held by the run until a PR exists, then by the held label on
   that PR.** Before the PR, there is nothing remote to label, so the claim lock
   holds the package's branch for the run. Once `ship` opens the PR, it carries
   the `in-progress` label, as a `pr-doctor` lease does. The lease is keyed on the
   branch (ADR-0019, decision 2), so `pr-doctor` skips the PR while it is held,
   and works it afterwards if it goes red.

## Alternatives considered

- **A GitHub Action that files tracker tickets for the `tickets` loop.** This
  needs no new Source. But each alert becomes a ticket that must be closed when
  Dependabot closes the alert. The Action needs a tracker credential in CI. The
  ticket Stages would not run the host check in decision 4, so a session could
  report the alert fixed while the lockfile still resolves a vulnerable version.
  Rejected.
- **Dependabot's own PRs.** These cost nothing to run. But Dependabot often
  cannot fix a pnpm transitive dependency, and it cannot write a
  `pnpm-workspace.yaml` override. Those are most of the alerts this loop is for.
  Rejected as the fix; Dependabot's PRs stay on for the alerts they can fix.

## Consequences

- A third built-in Source, and two new Steps: `fix` with the SecurityFix envelope
  on this Source, and `ship`. `ship` reuses `ship.Land`; the Steps after it are
  the existing `gates`, `watch_ci` and `retro`.
- The host check needs a lockfile reader for each package ecosystem it supports.
  pnpm's comes first. A Source configured for an ecosystem with no reader fails at
  load, not at the first alert.
- The SecurityFix session regenerates the lockfile, so it reaches the package
  registry. `post_create` already does, so this adds no new network reach. ADR-0002
  is reworded to say so: the sandbox holds no credential but Claude's, and it
  reaches the package registry, but it never reaches a remote the harness owns
  (git, the tracker, GitHub).
- An alert that Dependabot closes while the PR is open leaves the PR in place. A
  human merges or closes it; the next poll sees no alert and yields nothing.
- Glossary terms (the Source, the SecurityFix session) go in CONTEXT.md once the
  code lands, as ADR-0019's did.
