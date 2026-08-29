# ADR-0002: The harness owns all remote I/O; the sandbox is air-gapped except for Anthropic; the checkout is mounted at its real host path

- Status: Accepted
- Date: 2026-06-11
- Supersedes the GitHub-asymmetry decision in [ADR-0001](0001-harness-owns-linear-integration.md)

## Context

The harness now works a ticket through three single-role tools, each a separate
`cmd/` binary running one skill in its own ephemeral Docker sandbox:
`implementation` (`/tdd`), `review` (`/review-worktree`), `retrospective`
(`/retrospective`). The contract between them is the shared host checkout (the
worktree plus the ticket-keyed transcripts) and the `/findings/out.json` dropbox.

Two things forced a re-decision of how the sandbox touches the outside world and
where the checkout is mounted:

1. **ADR-0001 left GitHub asymmetric.** It kept `GH_TOKEN` *inside* the container
   so the review session could `git push` and (per the skill) raise the PR. That
   put a repo-scoped GitHub credential inside a `--dangerously-skip-permissions`
   session — the one credential class that can mutate the remote.
2. **The synthetic mount path made the worktree unusable from the host.** ADR-0001
   /the original design mounted the checkout at `/workspace/herd`. A git worktree
   records the repo path as an **absolute** path in its `.git` pointer, so a
   worktree created in-container stored `/workspace/herd/...`. That path does not
   exist on the host, so `git` inside the worktree dir failed on the host — a
   human could read the files but not `git diff` the live work, and worktree
   removal needed a throwaway container.

We also want review's "is this shippable?" judgement to rest on ground truth, not
the agent's self-report — and once push moves off the agent, *something* host-side
has to gate it.

## Decision

**The harness performs every remote interaction itself; the sandbox can reach
only Anthropic.** Concretely:

- **No `GH_TOKEN` in the container.** It is dropped from `sandbox.SecretEnv` and
  the `entrypoint.sh` HTTPS-push wiring is removed. Agents **commit locally only**,
  into the shared `.git`.
- **The harness pushes and opens the PR, host-side.** After the `review` container
  exits, the harness re-runs the quality gates (`pnpm check && pnpm typecheck`) on the
  branch in a throwaway container. **Only if they pass** does it
  `git -C $PROJECT_PATH push origin feat/beh-nnn` and `gh pr create`. The gate re-run
  is therefore both review's ground truth *and* the push gate — no branch reaches
  a PR on the agent's say-so. PR title/body are templated for now (ticket id +
  commit subjects); a Haiku PR-author session is deferred.
- **Push works from the main checkout, not the worktree.** The branch ref and
  objects live in the shared `.git`, which the host reads directly; the harness
  never needs to enter the worktree to push.
- **The checkout is bind-mounted at its real host path** (`-v $PROJECT_PATH:$PROJECT_PATH
  -w $PROJECT_PATH`) instead of `/workspace/herd`. Container-path == host-path, so the
  worktree's absolute `.git` pointer resolves identically in every container *and*
  on the host. A human can `cd .claude/worktrees/beh-nnn && git diff`; worktree
  removal runs host-side. The path is still fixed per machine, preserving the
  cross-container invariant the worktree contract depends on.

Combined with ADR-0001 (Linear), the rule is now uniform: **the harness owns all
remote I/O — Linear and git — and the sandbox holds only the Claude credential.**

## Alternatives considered

- **Keep `GH_TOKEN` in the container and let review push/PR (ADR-0001's stance).**
  Simplest, skill runs closer to verbatim — but leaves a remote-mutating
  credential in a skip-permissions sandbox, and makes review's success a
  self-report ("I pushed") rather than an independent gate. Rejected for blast
  radius and for weakening ground truth.
- **Trust the review session's own gate run; harness just pushes on exit 0.**
  Cheaper (no second gate container) but it is self-report: a review that silently
  broke the build would sail to a PR. Rejected — "never self-report" is the
  harness's core invariant.
- **Keep the synthetic `/workspace/herd` mount and inspect via the main checkout.**
  A human could still `git -C $PROJECT_PATH diff main...feat/beh-nnn`, but not work
  inside the live worktree, and removal still needed a container. The synthetic
  path bought nothing the real path doesn't also give (both are fixed per machine).
  Rejected.
- **Rewrite the worktree's gitdir pointers host-side for inspection.** Fragile,
  stateful, easy to leave half-rewritten. Rejected in favour of the real-path
  mount, which makes the problem not exist.

## Consequences

- The only secret inside the sandbox is the Claude credential
  (`ANTHROPIC_API_KEY` *or* `CLAUDE_CODE_OAUTH_TOKEN`). A fully compromised
  session cannot push, open a PR, or touch Linear; the worst case is a local-only
  mutation the harness's gate re-run catches before anything ships.
- One extra container run per ticket (the gate re-run). Accepted: it is the price
  of an independent, non-self-report push gate.
- The host's absolute path (e.g. `/Users/<name>/...`) now appears inside the
  container and in transcripts. Not a secret; a mild info leak accepted for host
  usability of worktrees.
- The image no longer needs `gh`, and `entrypoint.sh` no longer wires push auth.
- This **supersedes ADR-0001's GitHub asymmetry**: PR creation moves *out* of the
  sandbox to the host, matching how Linear is already handled. ADR-0001's Linear
  decision otherwise stands.
- `GH_TOKEN` is still required on the host (`config.go`) for the harness's push +
  `gh pr create`.
