# ADR-0021: A branch that edits the sandbox Dockerfile is gated in its own image

- Status: Accepted
- Date: 2026-10-25
- Extends: ADR-0008, ADR-0016

## Context

ADR-0008 lets a Consumer commit the Dockerfile for its sandbox image, and
`sandbox.Preflight` builds that image from the **checkout's** Dockerfile. It also
rebuilds the image when the Dockerfile's fingerprint changes. Every worktree
command ran in that image: the `post_create` prep and the host-side gate re-run
that authorises a push (ADR-0016).

A ticket whose fix *is* the sandbox Dockerfile, such as adding a missing
toolchain, could therefore never turn its own gate green. In-session `make check`
passed in both implementation and review. Then the host re-ran the gate in the old
image, went red for exactly the reason the branch fixes (exit 127, toolchain not
found), and pushed nothing (#61). This is a different problem from having no
docker in-session. Here the host has docker, but it builds from the wrong
Dockerfile.

## Decision

**When a worktree's copy of the configured `dockerfile` differs from the
checkout's, that worktree's commands (`post_create` and every gate) run in an
image built from the worktree's Dockerfile.**

- `sandbox.WorktreeImage` decides which image to use by comparing
  `DockerfileFingerprint` of `<worktree>/<dockerfile>` and
  `<checkout>/<dockerfile>`. A prebuilt `image` (no `dockerfile`), an unchanged
  Dockerfile, or a worktree with no Dockerfile all keep the configured image.
- The branch image is tagged `<image repo>:branch-<first 12 hex of the
  fingerprint>` (`sandbox.BranchImageTag`). It is keyed by content, not by branch
  name. Two branches with the same Dockerfile share one image, and a branch that
  edits its Dockerfile again gets a new one.
- `hostio.Runner.Shell` prepares it through the same `sandbox.Preflight` as the
  main image (disk floor, daemon, build on a miss, rebuild on a stale label),
  pointed at the worktree's Dockerfile and build context. The verdict is kept for
  the run, so the prep and all gates share one build, or one failed build.
- If the branch image cannot be built, nothing launches. The command's result is
  a terminal exit 125 whose stderr tail says the branch modifies the sandbox
  Dockerfile and gives the build error. The review stage already logs that tail
  next to the red gate, so an operator sees a broken Dockerfile, not a code
  failure.
- `ShellPreview` (`--dry-run`) names the branch image without building it.

The decision lives in the Runner, not in the Stages. Stages still name a worktree
and a command, and the Runner still owns the whole argv (ADR-0013).

## Alternatives considered

- **Only detect the case and label the red gate** ("gate red in the pre-change
  image; branch modifies the sandbox Dockerfile"). It is cheaper, but the ticket
  still can never ship by itself. An operator would have to merge it unverified
  and rebuild. Building the branch image costs one build and verifies the branch.
- **Decide from the branch's diff against `origin/main`.** That needs a git read
  in the sandbox layer. It also builds needlessly when the checkout is behind
  `origin/main` but already has the same Dockerfile. Comparing the two files on
  disk answers the real question directly: would the checkout's image be the
  wrong one?
- **Fingerprint the whole build context.** This repo's build context is
  `.agent-harness/`, but a Consumer's could be the repository root, and then
  every branch would build its own image. Only the Dockerfile's bytes are
  compared, the same fingerprint the stale-image rebuild already uses.

## Consequences

- A branch that changes only a file the Dockerfile `COPY`s still runs in the
  checkout's image. That is the same limit the stale-image rebuild has.
- The agent sessions (implementation, review, retrospective) still run in the
  checkout's image. The host gate is the ground truth that authorises a push, so
  it is the run that must see the branch's environment.
- The host now runs `docker build` over a Dockerfile an agent session wrote.
  That does not widen ADR-0002: `sandbox.BuildImage` passes no `--secret`,
  `--ssh` or `--build-arg`, a build step does not inherit the host's environment
  (so it never sees `GH_TOKEN` or a tracker credential), and the build context is
  the worktree's Dockerfile directory, so nothing outside the worktree can be
  `COPY`d in. Any future change that hands the build a credential must revisit
  this.
- Branch images are left unreferenced like the main image, so the daemon's
  `docker image prune -af` reclaims them. A merged Dockerfile change makes the
  checkout's own image stale, and `Preflight` rebuilds it on the next run.
