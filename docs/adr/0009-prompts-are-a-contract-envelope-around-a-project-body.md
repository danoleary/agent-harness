# ADR-0009: Stage prompts are a non-overridable contract envelope around a Consumer-supplied body

- Status: Accepted
- Date: 2026-07-01

## Context

Consumers want to use their own implement/review/retrospective prompts. But the
host-side jobs depend on invariants the *prompt* establishes inside the sandbox:
findings must be written to `/findings/out.json` in the `{title, body, kind, key}`
shape, work must land on the canonical `<branch_prefix>/<slug>` branch, and the
sandbox must not touch the tracker (ADR-0001/0010). Today these invariants and the
herd-specific body are one entangled Go string in `internal/prompt`.

A clarifying fact reshaped the request: **"when to post issues" and "when to create
PRs" are not in the prompt at all** — they are host-side Go (`internal/filing`,
`internal/stages/review.go`) that the sandbox can never reach. So those essentials
are fixed automatically; the only thing a custom prompt can break is the *contract*
the host-side jobs read back.

## Decision

**Every Stage prompt is composed as `contract envelope + Project body`, where the
envelope is harness-owned and non-overridable and the body is Consumer-supplied.**

- **Envelope (harness):** injected ticket context, the findings-dropbox protocol,
  the tracker-off steer, the branch/handoff contract, the sandbox bash-quirk steer,
  and — for the retrospective — the **sanitize rule** (Harness findings carry
  failure-class + harness-side detail only, never Consumer source/secrets; ADR-0011).
- **Body (Consumer):** `.agent-harness/prompts/{implement,review,retro}.md` — which
  skill to invoke, project conventions, gate hints. A Consumer that references a
  skill (e.g. `/tdd`) is responsible for that skill existing in its own repo.

`internal/prompt` stops hardcoding `/tdd`/`/review-worktree`/`/retrospective` and
instead reads the body from the bind-mounted `.agent-harness/prompts/`. The envelope
is the harness's public API; changing its contract is a breaking change.

## Alternatives considered

- **Consumer owns the whole prompt; harness appends a footer.** Simpler, but the
  body can contradict the envelope (tell the agent to open its own PR) with no
  enforcement. Rejected — the contract must be non-overridable.
- **No free-text; Consumer declares structured knobs and the harness generates the
  whole prompt.** Most controlled, but defeats "different prompts." Rejected.

## Consequences

- Consumers get real prompt freedom without being able to break push/PR/filing.
- The host-side "when to post issues / create PRs" logic needs no per-Consumer
  configuration — it is structurally out of the agent's reach.
- If a Consumer's body omits the skill invocation entirely, the Stage still runs
  under the envelope; quality is the Consumer's problem, the contract is not.
