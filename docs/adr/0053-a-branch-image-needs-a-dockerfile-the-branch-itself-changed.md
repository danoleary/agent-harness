# ADR-0053: A branch image needs a Dockerfile the branch itself changed

- Status: Accepted
- Date: 2026-11-14
- Extends: ADR-0022

## Context

ADR-0022 runs a worktree's `post_create` and gates in an image built from the
worktree's Dockerfile when that file's bytes differ from the checkout's. It
rejected deciding from the branch's diff against `origin/main`, because that
diff would build needlessly when the checkout is behind `origin/main` but already
has the branch's Dockerfile.

The byte comparison has the opposite blind spot. A re-dispatched ticket reused a
worktree with no commits ahead of the base it was cut from. Meanwhile
`origin/main`, and the checkout after it, took a release commit that changed the
sandbox Dockerfile. The two files differed, so the runner logged "branch modifies
the sandbox Dockerfile" and built a branch image from the **older** Dockerfile.
The branch's prep and gates then ran on a superseded toolchain, and a failed
build of that stale file would have failed the run (#333).

## Decision

**A worktree runs in a branch image only when its Dockerfile differs from the
checkout's AND differs from its own branch point.**

- The branch point is `git merge-base HEAD origin/main` in the worktree.
  `sandbox.WorktreeImage` asks `git diff --name-only <branch point> -- <dockerfile>`,
  which compares the worktree's file with that commit, so it counts the branch's
  commits and any uncommitted edit to the Dockerfile.
- The byte comparison with the checkout stays as the first test. It is cheap,
  and it still covers the case ADR-0022 rejected the diff for: a branch whose
  Dockerfile change the checkout already has runs in the checkout image.
- If git cannot answer (no repository, no `origin/main`), the byte comparison
  alone decides, as before. That keeps #61's guarantee: a branch that fixes its
  own sandbox is never gated in the pre-change image just because a git read
  failed.

## Consequences

- A stale branch that never touched the Dockerfile runs in the checkout image,
  which is the one main ships. Nothing is built, and the run log no longer says
  the branch modified the Dockerfile.
- A stale branch that did change the Dockerfile still builds its own file, which
  does not include main's later change to it. The harness rebases the branch
  before it pushes, and the gate that authorises the push (ADR-0016) runs on the
  rebased tree.
- `WorktreeImage` now runs two local git reads per call, each bounded by a
  timeout. They only happen when the two files already differ.
