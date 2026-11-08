# Fix Dependabot alerts with `security-alerts`

Dependabot opens its own PRs, but it often cannot move a transitive dependency to
a patched version, and it never writes a pnpm `overrides` entry. Those alerts stay
open until someone writes the fix. The built-in `security-alerts` loop writes it:
it reads the repository's open Dependabot alerts on the host, and lands one PR
per vulnerable package (ADR-0040).

## Before you run it

- **Dependabot alerts are on for the repository**: Settings → Code security →
  Dependabot alerts.
- **`GH_TOKEN` can read them.** A classic token needs the `security_events` scope;
  the `repo` scope the harness already asks for includes it. A fine-grained token
  needs the "Dependabot alerts: Read-only" repository permission. See
  [token scopes](../reference/tokens.md).
- **`origin` is the project's GitHub repository.** The loop reads the alerts of
  that repository.

When `.agent-harness/config.toml` declares a `github_security_alerts` loop
([below](#change-the-severities-or-the-prompt)), `agent-harness doctor` checks
all three with one read of the alerts. It reports a token that can't read them,
with the scope or permission to add, and alerts that are turned off on the
repository. The built-in loop alone adds no `doctor` line, since it runs only
when you name it.

## Run it

```bash
agent-harness loop security-alerts --dry-run   # the packages it would work, and why it skips the rest; changes nothing
agent-harness loop security-alerts             # run it in the foreground
agent-harness start security-alerts            # or as a detached daemon (loop-security-alerts.pid, loop-security-alerts.log)
touch "$PROJECT_PATH/.agent-harness/STOP-security-alerts"   # wind it down
```

It logs to `.agent-harness/logs/loops/security-alerts/`; follow it with
`agent-harness watch --loop security-alerts`.

## What it does with one package

The Work item is an alert group: every open alert on one package, at the loop's
severities. For each group the loop:

1. cuts `<branch_prefix>/sec-<ecosystem>-<package>` fresh from `origin/main`
   (`new_branch`);
2. runs one sandboxed SecurityFix session that bumps the dependency, or adds an
   override, and regenerates the lockfile (`fix_alerts`);
3. checks on the host that the diff touches dependency files only and the
   lockfile no longer resolves a vulnerable version, then runs every Gate
   (`gates`);
4. opens a PR that lists the alerts it fixes and carries the `in-progress` label
   while the run holds it (`open_pr`);
5. watches CI, and fixes a red check with a gated CI fix (`watch_ci`), then files
   the run's Findings (`retro`).

Nothing is pushed if a check fails. It fixes npm (pnpm) and Go modules; the
[loop guide](declare-a-loop.md#run-or-re-tune-the-security-alerts-loop) lists
the files each one scans and may change.

## What it leaves alone

`--dry-run` prints the reason for every package it skips. It skips a package when:

- none of its alerts has a patched version (an alert with no patch is left open
  in the PR body of a package that has others);
- its ecosystem, or the manifest an alert names, is not one it can check;
- its branch already has an open PR, or a PR labelled `blocked`;
- a human closed a PR for the same alerts, or a merged fix's alerts are still
  open after a day (inside the day it waits for Dependabot to close them);
- it already tried these alerts at the current `origin/main`, or tried them
  `max_attempts` times (2 by default). Then it hands the package to a human. A
  new alert, or a changed advisory, starts a fresh budget.

The attempts are kept in `.agent-harness/logs/loops/security-alerts/attempts.jsonl`.

## Change the severities or the prompt

The built-in loop works `critical` and `high` alerts. Write out a `[[loops]]`
entry named `security-alerts` to change that, or to give `fix_alerts` a project
prompt body; see [declare a loop](declare-a-loop.md#run-or-re-tune-the-security-alerts-loop).
