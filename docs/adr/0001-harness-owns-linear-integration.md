# ADR-0001: The harness owns all Linear I/O; nothing Linear enters the sandbox

- Status: Superseded by ADR-0010
- Date: 2026-06-11

> Superseded by [ADR-0010](0010-tracker-is-a-port-with-linear-jira-github-adapters.md):
> the host-owns-all-tracker-I/O invariant below is retained, but generalized from
> Linear specifically to a `Tracker` port with Linear/Jira/GitHub adapters.

## Context

The agent harness runs two Claude Code skills per ticket — `/tdd` and
`/review-worktree` — each inside an ephemeral Docker sandbox so Claude can run
with `--dangerously-skip-permissions`. Both skills, as written, reach Linear
through the **official Linear MCP server** (`mcp__linear-server__*`): tdd claims
the ticket (→ In Progress), and review reads the issue to reconstruct intent and
files systemic findings via the triage skill.

That server authenticates by **interactive OAuth**. In a headless container there
is no browser to complete the flow, and a prior decision in this codebase already
records that "interactively-authenticated MCP servers may be absent in headless/
cron runs."

We chose **not** to mount the host `~/.claude` directory into the sandbox (which
would have carried a warm Linear OAuth token alongside Claude's own credentials).
Instead the operator passes a dedicated `ANTHROPIC_API_KEY` for Claude and a
`GH_TOKEN` for GitHub. That removes the one easy path to a working in-container
Linear MCP, forcing the question: how do the skills touch Linear?

## Decision

**The harness performs every Linear interaction itself, via the Linear GraphQL
API with a `LINEAR_API_KEY` that lives only on the host. No Linear access of any
kind is configured inside the sandbox.** The skills are steered off their MCP
steps via their prompts:

- **Claim:** the harness moves the ticket → In Progress *before* launching tdd;
  the `/tdd` prompt says the ticket is already claimed and to not call Linear.
- **Reconstruct intent:** the harness fetches the ticket (title, body, acceptance
  criteria) and injects it directly into the `/review-worktree` prompt.
- **Harness-improvement findings (both sessions):** `tdd` (its retrospective) and
  `review-worktree` (its systemic-findings step) both append findings to a
  mounted findings-dropbox file (`/findings/out.json`) instead of filing them;
  after **every** session the harness reads the dropbox and files one Linear issue
  per finding via the API.

GitHub is treated asymmetrically: **PR creation stays in the sandbox** (review
keeps using `gh` with `GH_TOKEN`), because composing a PR is content-heavy and
session-local, whereas Linear updates are simple state operations the harness
already performs as part of selection and claiming.

## Alternatives considered

- **Mount the whole `~/.claude` into the container.** Carries Claude + Linear
  OAuth in one move and lets the skills run verbatim, but puts all the operator's
  credentials inside a `--dangerously-skip-permissions` sandbox, and depends on a
  token that can expire/refresh mid-run. Rejected for blast radius and fragility.
- **Mount only the single Linear OAuth credential file.** Smaller surface than
  the whole directory, skills still verbatim — but still a credential mount and
  still subject to mid-run token expiry. Rejected in favour of zero Linear in the
  sandbox.
- **A third-party / API-key Linear MCP server.** Could authenticate by
  `LINEAR_API_KEY`, but its tool names would differ from the official
  `mcp__linear-server__*` the skills call by name, breaking the skills. Rejected.

## Consequences

- The only secrets inside the sandbox are `ANTHROPIC_API_KEY` and `GH_TOKEN`.
  Linear access (the `LINEAR_API_KEY`) never crosses the container boundary.
- Linear control is centralized in the harness, consistent with selection,
  blocked-status checks, and breadcrumb comments already living there.
- **The skills no longer run fully verbatim under the harness** — their prompts
  must steer both off `mcp__linear-server__*`. If a skill's Linear steps are
  reworked upstream, the harness prompts must track that change. This is the main
  cost and the reason this is a recorded, not incidental, decision.
- Filing harness-improvement findings now depends on the sessions writing the
  findings-dropbox file; if a session writes nothing, none are filed (the common,
  friction-free case). A malformed dropbox is logged but not filed (a degraded,
  not broken, outcome).
