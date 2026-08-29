# ADR-0008: Project configuration and the adapter surface live in the Consumer repo

- Status: Accepted
- Date: 2026-07-01

## Context

Once the harness is a standalone tool (ADR-0007), every project-specific thing it
assumes today must come from somewhere: which tracker, the gate commands, the
sandbox image, the branch prefix, the toolchain setup, the prompt bodies, and the
feedback policy. That configuration can either travel *with the harness invocation*
(env/flags/a central per-project file the maintainer keeps) or *with the project*
(committed into the repo being worked).

## Decision

**Project configuration lives in the Consumer repo, in a committed
`.agent-harness/` directory, read by the harness from the bind-mounted checkout.
Only secrets and host paths stay as env vars on the harness side** (the existing
`LINEAR_API_KEY`/`GH_TOKEN`/Claude-credential/`PROJECT_PATH` model is unchanged). The
adapter surface a Consumer declares:

- `.agent-harness/config.*` — tracker selection + non-secret tracker fields
  (team/project/queue/label/state names), the named **Gate** list (ADR-0009),
  sandbox `image` *or* `dockerfile` + an optional cache volume+path, `branch_prefix`
  (default `feat`), a `post_create` hook, and the `feedback.upstream` policy
  (ADR-0011).
- `.agent-harness/prompts/{implement,review,retro}.md` — the **Project bodies**
  (ADR-0009).
- `.agent-harness/Dockerfile` (optional) — the sandbox image, `FROM` the published
  base (ADR-0007). A Consumer with no `image`/`dockerfile` **hard-errors** — there
  is no sane cross-language default.

The harness owns the **worktree/branch lifecycle host-side** (it creates
`<branch_prefix>/<slug>` and keys verify/push/PR/dispatch-guards off it); the
Consumer owns only the *toolchain setup inside* that worktree via `post_create`.
This retires the fragile "the sandbox agent runs `new-worktree.sh`, and the host
depends on its output" coupling: git creation moves into the harness, the
herd-specific `pnpm install`/`tokens:build`/Playwright steps become herd's
`post_create` hook.

## Alternatives considered

- **Central per-project config the maintainer holds (env/flags).** Works for repos
  you don't control, but config drifts from the code and you maintain N configs.
  Rejected — config should version with the code it drives.
- **Keep delegating worktree creation to a Consumer script.** Closer to today but
  keeps host correctness hostage to a sandbox step. Rejected.

## Consequences

- "Use the harness on a new project" = "add `.agent-harness/` to that repo," never
  "fork the harness."
- Gates are language-agnostic shell commands, so Go/.NET/Node Consumers differ only
  in config, not in harness code.
- Dispatch guards (`TicketAlreadyOnMain`, the resumed-branch advisory) get
  parameterized by **Key** + `branch_prefix` instead of hardcoded `BEH`/`feat`.
- A Consumer can strand work only by mis-declaring `branch_prefix`; the host now
  owns the canonical branch, so the old `fix/<slug>` silent-strand class is closed.
