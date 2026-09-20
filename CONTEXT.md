# Agent Harness — Context

The shared language of the harness *as a standalone, multi-project tool* (see
ADR-0007). It is deliberately generic: nothing here names herd, Linear, or pnpm,
because the whole point of the extraction is that those are *consumer* choices,
not harness concepts. The design narrative lives in [`docs/DESIGN.md`](docs/DESIGN.md);
this file fixes the vocabulary.

## Language

### Core

**Harness**:
The standalone Go program that works a backlog one ticket at a time through three
stages, owning all remote I/O on the host.
_Avoid_: agent, bot, runner.

**Consumer** (a.k.a. **Project**):
A repository that uses the harness by committing an `.agent-harness/` directory.
herd is the first Consumer, not a special case.
_Avoid_: client, host (host = the machine running the harness, a different thing).

**Stage**:
One of the three single-role Claude sessions run per ticket — **Implementation**,
**Review**, **Retrospective** — each in its own ephemeral sandbox.
_Avoid_: step, phase, job (job is overloaded; "stage" is the unit).

**Key**:
The tracker-agnostic ticket identifier (`BEH-123`, `PROJ-123`, `#123`) the harness
slugs branches from and greps history with. The harness treats it as an opaque
string supplied by the Tracker adapter.
_Avoid_: ticket id, BEH number, issue number.

### Prompts

**Contract envelope**:
The non-overridable prompt scaffolding the harness wraps every Project body in —
the findings-dropbox protocol, the tracker-off steer, the branch/handoff contract,
the sanitize rule, and the injected ticket context. The envelope is the harness's
public API surface (ADR-0009).
_Avoid_: wrapper, header, system prompt.

**Project body**:
The per-stage prompt text a Consumer supplies in `.agent-harness/prompts/`. Owns
which skill to invoke and the Consumer's conventions; cannot delete the envelope.
_Avoid_: prompt (ambiguous — a Stage prompt is envelope + body).

### Verification

**Gate**:
A named, Consumer-declared shell command whose green exit is a precondition the
host checks before it pushes and opens the PR. Language-agnostic (`go test ./...`,
`dotnet test`, `pnpm check`).
_Avoid_: check, test (those name specific gates, not the concept).

**Ground truth**:
A host-side, tracker-agnostic git assertion (worktree exists, commits ahead of
base, clean tree) the harness verifies independently of what the sandbox reported.

**Disposition**:
A run's one terminal verdict — **Shipped**, **RecommendClose**, **CapAborted**,
**PreflightAborted** or **NoPR** (`stages.Disposition`). Exactly one holds per
stage and, folded across the three, per ticket; it is the only thing the loop
branches on and the breaker counts. Distinct from a Stage's **OK**, which asks
only "did this stage do its job?" — a Shipped run can be not-OK (CI red after the
auto-fix budget) and an OK implementation is still NoPR (it never pushes).
_Avoid_: outcome, signal, status.

### Sandbox

**Sandbox base image**:
The harness-published image carrying the Claude CLI, `git`, the uid-re-exec
entrypoint, and the bind-mount contract. A Consumer's image layers its toolchain
`FROM` it (ADR-0008).

**post_create hook**:
A Consumer-declared command the harness runs after it creates the worktree, to
install the toolchain (deps, generated files, browsers, env links).

### Feedback

**Finding**:
A retrospective observation written to the **findings dropbox**, classified by
**audience**: a **Project finding** is about the Consumer's own codebase/CI and
routes to its Tracker; a **Harness finding** is about the harness/sandbox/contract
itself and routes to the Upstream sink (ADR-0011).
_Avoid_: issue (a Finding becomes an issue only after the host files it).

**Findings dropbox**:
The mounted `/findings/out.json` file the sandbox writes Findings to; the host
reads, dedups, and files it after the session. The sandbox never reaches a remote.

**Upstream sink**:
The public, harness-maintainer-owned GitHub repo where opted-in **Harness
findings** are filed as issues (default off; ADR-0011).

## Relationships

- A **Consumer** commits one `.agent-harness/`; the **Harness** runs three
  **Stages** per **Key**.
- Each **Stage** prompt = **Contract envelope** + **Project body**.
- A **Tracker** adapter resolves a **Key** to ticket context and owns
  claim/release/select; one of Linear, Jira, GitHub Issues (ADR-0010).
- A **Stage** writes **Findings** to the **Findings dropbox**; the host classifies
  each as **Project** (→ Tracker) or **Harness** (→ **Upstream sink**, if opted in).
- The host runs every **Gate** in the Consumer's sandbox image and pushes + opens
  the PR only if all are green.

## Example dialogue

> **Maintainer:** "When a **Stage** finds the sandbox is missing a system library,
> is that a **Project finding** or a **Harness finding**?"
> **Designer:** "Harness — it's about *our* base image, so it routes to the
> **Upstream sink**. A failing `pnpm check` because the Consumer's own code is
> broken would be a **Project finding** and stay in their **Tracker**."
> **Maintainer:** "And if the Consumer never opts in to upstreaming?"
> **Designer:** "The Harness finding stays a local artifact in their repo. Nothing
> leaves without consent — the **Upstream sink** is public."

## Flagged ambiguities

- "host" was used for both the machine running the harness and the consuming repo.
  Resolved: **host** = the machine/process doing remote I/O; **Consumer** = the
  repo being worked. ADR-0001 used "host" only in the first sense.
- "job" was used for both a Stage and the whole pipeline. Resolved: **Stage** is
  the single-role unit; the three together are the **pipeline**.
- "Linear" was used as a synonym for the issue tracker. Resolved: Linear is one
  **Tracker** adapter; the concept is **Tracker** (ADR-0010).
