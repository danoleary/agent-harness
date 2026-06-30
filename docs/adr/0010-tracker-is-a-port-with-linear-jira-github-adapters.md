# ADR-0010: The issue tracker is a port with Linear, Jira, and GitHub Issues adapters

- Status: Accepted
- Supersedes: ADR-0001 (which scoped the same ownership to Linear specifically)
- Date: 2026-07-01

## Context

ADR-0001 decided the harness owns all *Linear* I/O on the host, with nothing
tracker-related inside the sandbox. That ownership decision is still right, but it
is hardcoded to Linear: the GraphQL client, the `agent-harness` label UUID, the
`ready-for-agent` state, and the `BEH-NNN` key shape are baked into
`internal/linear` and `internal/filing`. Consumers need Linear, **Jira, or GitHub
Issues**.

## Decision

**Introduce a `Tracker` port, host-side only, with three adapters — Linear, Jira,
GitHub Issues — selected by `.agent-harness/config` (ADR-0008).** The port is the
lowest common denominator of the three layers the harness actually uses:

1. **Ticket source** — resolve a **Key** to title/body/children.
2. **Queue + claim/release** — select the next ready ticket; move it to/from an
   in-progress state (Linear state / Jira transition / GitHub assignee+label).
3. **Findings sink** — file + dedup Project findings (Harness findings route
   elsewhere; ADR-0011).

The harness's notion of a ticket becomes a tracker-agnostic **Key** (`BEH-123`,
`PROJ-123`, `#123`) — an opaque string the adapter produces and the harness slugs
branches from and greps `main` history with. ADR-0001's core invariant is retained
and generalized: **the harness performs every tracker interaction itself; no tracker
credential or MCP ever enters the sandbox.**

## Alternatives considered

- **Linear-only, parameterized.** Cheapest, zero abstraction risk — but the stated
  requirement is Linear *and* Jira *and* GitHub Issues, so a real port is needed.
- **Shell out to Consumer-provided tracker scripts.** Maximum flexibility, but
  pushes the claim/dedup/select complexity onto every Consumer. Rejected — the
  harness owns this (ADR-0001) and three first-party adapters cover the field.

## Consequences

- The interface must stay LCD: capabilities unique to one tracker (Linear
  sub-issues vs Jira sub-tasks vs GitHub task-lists) are normalized to "children"
  or dropped, not surfaced.
- "Ready for the agent" is expressed per-adapter in config (Linear state name, Jira
  status, GitHub label/column) — there is no universal queue concept.
- The per-Stage CLI tools can run with a no-tracker/manual mode (Key + ticket text
  supplied directly), useful for local runs and for Consumers without a tracker.
