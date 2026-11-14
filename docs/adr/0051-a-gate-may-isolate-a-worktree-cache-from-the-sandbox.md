# ADR-0051: A gate may isolate a worktree cache from the sandbox

- Status: Accepted
- Date: 2026-11-14
- Extends: ADR-0016 (the host gate re-run is the push authority)

## Context

The host gate re-runs each `[[gates]]` command in a throwaway container that
bind-mounts the whole checkout, so it sees the worktree exactly as the sandbox
session does. Some caches live inside the worktree. herd's Storybook gate
rebuilds the Vite dependency cache under `web/node_modules/.vite`, which the
session also writes. When the gate rebuilt the cache the session had written,
every story file could fail (#314, part of #306). Nothing about the diff caused
the red.

## Decision

**A `[[gates]]` entry may list `cache_isolate` worktree-relative directories.
Each run of that gate mounts a fresh, empty tmpfs over each one, in that gate's
container only.**

- **Where.** `sandbox.BuildWorktreeCommandArgs` adds
  `--mount type=tmpfs,dst=<worktree>/<path>,tmpfs-mode=1777` after the checkout
  bind. Every host gate run is built by `fix.GateRun`: the base gate, the review
  gate, and the PR and security loops' re-gates. So they all isolate the same
  paths. `post_create` and the agent sessions do not.
- **Why tmpfs.** It starts empty, needs no cleanup, and goes away with the
  container. Mode 1777 lets the unprivileged runtime uid write to it. Its cost
  is memory: the tmpfs counts against the container's memory, so the key is for
  cache directories, not dependency trees.
- **The mountpoint is made host-side first.** If the directory is missing,
  docker creates the mountpoint through the bind as root. The session then could
  not write its own cache there. So `Runner.Shell` creates each path as the
  operator first, unless the worktree is gone.
- **Validation.** A path must be non-empty, relative, stay inside the worktree
  after cleaning (not `.`, not `..` out of it), and carry no comma, which would
  end the `--mount` value. A bad path fails config load.
- **Default.** With the key absent, the argv is what it was.

## Alternatives considered

- **A fresh host temp dir bind-mounted over the path.** It is on disk, so memory
  is not a concern, but the harness would have to create it, clean it up and
  own it for every attempt. On macOS it also depends on Docker Desktop sharing
  the temp dir.
- **Point the cache elsewhere with an env var.** That depends on the tool (Vite
  reads `cacheDir` from its config, not the environment), so the harness could
  not do it for any Consumer.
- **Have the gate command delete the cache first.** That deletes the session's
  copy too, which is the sharing this decision removes.
