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
   The branch is reset to `origin/main` even when a failed attempt left one of
   that name (`Worktree.CreateFresh`): the ticket loop's re-attach would bring
   that attempt's commits back.
4. **The host checks the result before any Gate runs.** The session's
   self-report is not trusted (ADR-0016). The branch must have a committed diff,
   and the diff may touch only dependency files: manifests, lockfiles, and the
   workspace file that holds overrides. For pnpm these are `package.json` (at
   any depth), `pnpm-lock.yaml` and `pnpm-workspace.yaml` (`verify.SecurityFix`).
   The lockfile must no longer resolve any version of the package inside any of
   the item's alerts' vulnerable ranges. A result that fails either check pushes
   nothing.
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
  on this Source, and `ship`. `ship` reuses the Landing through `ship.LandAs`,
  which takes the PR's title and body from its caller, since there is no ticket
  to build them from; the Steps after it are the existing `gates`, `watch_ci`
  and `retro`.
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

## Amendment, 2026-11-07: the Source's skips, fingerprint and ledger (#200)

`internal/secloop` implements the Source. It pins down decisions 1, 6, 7 and 8:

- **The Work item.** The Key is `SEC-<ecosystem>-<package>`, and the branch is
  `<branch_prefix>/<slug of the Key>`.
- **The fingerprint** is the first 10 hex digits of a SHA-1 over the package,
  its patched alerts' numbers in order, and each alert's first patched version.
  Decision 7 named the vulnerable ranges. The patched version is what the fix
  moves to, and a changed advisory moves it too.
- **The skips, cheapest first.** The Source skips a package when:
  - its ecosystem or a manifest has no lockfile reader;
  - none of its alerts has a patched version;
  - its branch has an open PR;
  - a PR from its branch carries the `blocked` label;
  - a human closed a PR whose body marker names this fingerprint;
  - a PR with this fingerprint merged, and its alerts are still open. Inside a
    grace period the Source waits for Dependabot. Past it, a human must look.
  - this fingerprint was already handed off;
  - an attempt at this fingerprint started at the current `origin/main` SHA.
    It pushed nothing, or it would have a PR, so it is not retried until main
    moves.

  The dry-run prints each skip's reason.
- **The ledger** is `<logs dir>/attempts.jsonl`. Each line has `key`, `fp`,
  `base_sha`, `at` and `outcome`. Holding the lease writes a `started` line, which
  is one attempt. When a fingerprint reaches `max_attempts`, the Source writes
  one `handed_off` line and a `ticket-released` stream record, then leaves the
  package alone until its fingerprint changes. Unlike `pr-doctor`, there is no PR
  label for a human to remove.
- **Claims** are the open PRs on `sec-` branches that carry the held label, each
  timed by its Key's latest `started` line. A labelled PR the ledger never
  started is left out, as in #196. Release takes the label off.

## Amendment, 2026-11-07: the loop as config declares it (#201)

The loader accepts the Source and `agent-harness loop <name> --dry-run` prints
its plan. A real run is refused as not yet runnable until the Pipeline lands.
The config differs from the Decision's example in three ways:

- **The Source key is `severity`**, not `severities`. It takes `critical`,
  `high`, `medium` and `low`, and defaults to `[critical, high]`.
  `max_attempts` is the per-fingerprint budget and defaults to 2, not
  `PR_LOOP_MAX_ATTEMPTS`: a fingerprint is a package's whole alert set, not a PR.
  `label`, `scope` and `when` are `github_prs` parameters and are rejected.
- **The Steps have their own names.** `new_branch` cuts the branch fresh
  (decision 3). `fix_alerts` runs the SecurityFix session and takes its prompt.
  `open_pr` is the Landing as a new PR. Reusing `fix` and `ship` would have given
  one step name two checks, depending on the Source.
- **The order is fixed:** `new_branch`, `fix_alerts`, `gates`, `open_pr`, then
  optionally `watch_ci` and `retro`. The first four are required.

```toml
[[loops]]
name = "security-alerts"
source = { kind = "github_security_alerts", severity = ["critical", "high"] }
steps = [
  { use = "new_branch" },
  { use = "fix_alerts", prompt = "prompts/security-fix.md" },
  { use = "gates" },
  { use = "open_pr" },
  { use = "watch_ci" },
  { use = "retro" },
]
```

## Amendment, 2026-11-07: the Pipeline (#203)

`secloop.Fix` runs the Steps, and `agent-harness loop <name>` now runs the loop
for real. It pins down decisions 3 to 8:

- **`new_branch`** runs the preflight, fetches `origin/main`, cuts the branch
  with `Worktree.CreateFresh`, and runs `post_create`. The PR loop's
  provisioning moved to `fix.Env.Provision`, so both loops share it.
- **`fix_alerts`** is one SecurityFix session, given every alert on the
  package, including the ones with no patch, which it is told to leave alone.
  `verify.SecurityFix` checks the result against a scan of the worktree's root
  `pnpm-lock.yaml` over the patched alerts' vulnerable ranges. A lockfile that
  cannot be read fails the check.
- **`gates`** is `fix.Env.Gates`, as on `pr-doctor`.
- **`open_pr`** is `ship.LandAs` with no conflict hook. Its title is
  `fix(deps): patch <pkg> (<n> <severities> alerts)`. Its body has the alert
  table with links, the alerts left open because no patch exists, the commits,
  and the fingerprint marker. A landed PR gets the held label through the
  host's `LabelPR`, which uses the REST labels endpoint because that endpoint
  creates a label the repository lacks.
  - A **conflict** pushes nothing. The run's `started` line already keeps the
    package from being retried until main moves.
  - A **collapse** means main already fixed the package. The run writes a
    `resolved` ledger line and ends as recommend-close. The Source then skips
    that fingerprint, even after main moves. It waits the grace period for
    Dependabot to close the alerts, and after that a human must look, as with a
    merged fix.
- **`watch_ci`** watches CI but fixes nothing. A CIFix on a dependency bump
  would have to touch more than dependency files. The run ends by taking the
  held label off, so `pr-doctor` can then work a red PR.
- **The Source refetches `origin/main` before every poll.** The daemon fetches
  only at startup and after an item. A loop that skips every package until main
  moves would otherwise never see main move.

## Amendment, 2026-11-08: built in as `security-alerts`

Once the Pipeline ran, nothing kept the loop out of `config.BuiltInLoops()`: it
was left undeclared only because it could not run when #201 shipped. It is now
the fourth built-in loop, `security-alerts`, with the defaults a declared entry
gets (`severity = ["critical", "high"]`, `max_attempts = 2`) and the steps
`new_branch`, `fix_alerts`, `gates`, `open_pr`, `watch_ci`, `retro`. A built-in
runs only when it is named, so this changes what `loop --list` and `--help` show,
not what a project runs. An entry named `security-alerts` replaces it, as for the
other built-ins.
