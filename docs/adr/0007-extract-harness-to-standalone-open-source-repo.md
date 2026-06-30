# ADR-0007: Extract the harness into a standalone, open-source repo; herd becomes a Consumer

- Status: Accepted
- Date: 2026-07-01

## Context

The harness lives at `herd/agent-harness/` as a subdirectory with its own
`go.mod` (`github.com/beherd/agent-harness`, stdlib only). It never imports herd
source — herd is purely bind-mounted at runtime — so the coupling is not in the
code but in a handful of assumptions: a Linear-only tracker, a `pnpm` gate, a
herd Dockerfile, a `feat/<slug>` branch, and `scripts/new-worktree.sh`. We want
to run the harness against other projects (different languages: Go, .NET, …) and
keep improving it from how it behaves *there*, not only on herd.

## Decision

**The harness moves to its own standalone, open-source repository. herd stops
vendoring it and becomes an ordinary Consumer** (a repo that commits an
`.agent-harness/` directory; see ADR-0008). The extraction is the umbrella under
which ADR-0008 (config in the Consumer repo), ADR-0009 (prompt envelope/body),
ADR-0010 (Tracker port), and ADR-0011 (feedback to the public repo) hang.

- **Distribution.** Primary path is **prebuilt cross-platform binaries on GitHub
  Releases**, because target Consumers are not all Go shops and must not need a Go
  toolchain. `go install` stays as a convenience; the **Sandbox base image is
  published to GHCR** (ADR-0008). The harness host still requires `docker`, `git`,
  and `gh`.
- **Migration.** Extract **with git history** (`git filter-repo
  --subdirectory-filter agent-harness`) so the ADRs, `DESIGN.md`, and the BEH-tagged
  commit knowledge survive; rename the module to the new org path. herd then deletes
  the subtree and adds its own `.agent-harness/` Consumer config.
- **Open source.** The repo is public so that the feedback loop (ADR-0011) can file
  findings as issues on it, and so herd's dogfooding flows upstream like any other
  Consumer's.

## Alternatives considered

- **Keep it a herd subdir, copy it per project.** Each project drifts; no shared
  improvement loop. Rejected — the whole goal is one harness, many Consumers.
- **herd keeps the source via git submodule.** Avoids a release pipeline but forces
  every Consumer to carry a Go toolchain and build, and makes herd a privileged
  co-located copy rather than a peer Consumer. Rejected for binary releases.
- **Fresh repo without history.** Simpler extraction, but discards the hard-won
  rationale captured in commits/ADRs. Rejected.

## Consequences

- herd loses its in-tree harness; harness changes ship via release, not a herd PR.
  Dogfooding now means herd consumes a released artifact like everyone else.
- A public repo means Harness findings must be sanitized before upstreaming
  (ADR-0011) — proprietary Consumer code must never land in a public issue.
- The module path change is a one-time break for anything importing the package.
