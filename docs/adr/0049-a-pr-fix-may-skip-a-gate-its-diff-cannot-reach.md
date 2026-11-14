# ADR-0049: A PR fix may skip a gate its diff cannot reach

- Status: Accepted
- Date: 2026-11-14
- Amends: ADR-0016 (a skipped gate authorises a PR loop push, and CI is the check)

## Context

A pr-doctor conflict fix on PR-217 changed one line of a GitHub Actions
workflow (two adjacent action-pin bumps). The host re-gated it with the full
`make check`, about 14 minutes, and then `make build`. CI then ran the same
checks again after the push. Nothing the Go gate checks reads
`.github/workflows/`, so the local run could not catch anything the change
could break. `.github/dependabot.yml` now produces bumps like this every two
weeks, and each one held up the PR loop for a full gate run (#221).

The docs-only short-circuit (BEH-687) does not cover this. It skips the gates
and the CI watch only for prose no gate *or CI job* reads. A workflow file is
the CI job, so the harness must still watch CI for it. Only the host gate is
redundant.

## Decision

**A `[[gates]]` entry may list `skip_when_only` glob paths. When every path in
a PR loop fix's diff against origin/main matches one of them, the PR loop skips
that gate host-side, logs it and why, and pushes. CI must pass before merge.**

- **Who honours it.** Only the PR loop (`pr-doctor`, any `github_prs` loop),
  for its conflict resolutions and its CI fixes (`fix.Env.SkipWhenOnly`). The
  implementation and review stages and the security-alerts loop ignore the key
  and run every gate. A skipped gate is also skipped over a CI fix's trial
  merge with origin/main. The red-base check still gates origin/main with every
  gate, because its verdict is shared with the ticket loop.
- **What it matches.** `git diff --name-only origin/main` over the worktree,
  the same diff the docs-only check reads. A diff that cannot be read, or is
  empty, skips nothing. A path is matched segment by segment: `path.Match`
  within a segment, and `**` for any number of whole segments.
- **Why a push with no local gate is safe here.** The PR is already open, and
  main's ruleset requires the `Check, build` status on an up-to-date branch
  before merge. A red CI run sends the PR back to the PR loop's own CI watch.
  A Consumer should set the key only where a required check runs the same gate.
- **Default.** With the key absent, nothing changes. This repo sets
  `skip_when_only = [".github/**"]` on `check`. `build` stays unlisted.
- A bad glob (empty, absolute, or one `path.Match` rejects) fails config
  validation, so it cannot silently never match.

## Alternatives considered

- **Skip host gates for a conflict merge whose resolved diff matches one side
  verbatim.** That covers conflicts only, not a CI fix to a workflow. It also
  ties the skip to how the conflict was resolved, not to what the gate can
  see.
- **Keep the full gate.** About 14 minutes per Dependabot PR for a check CI
  repeats anyway.
- **Honour the key in the review stage too.** A ticket's first push is the
  first time CI sees its branch, and the review gate is the last local check
  before a PR is opened. The maintainer kept that unchanged.
