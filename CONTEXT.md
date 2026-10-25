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
**Review**, **Retrospective** — each in its own ephemeral sandbox. In a loop's
config each Stage is a **Step** (`implement`, `review`, `retro`).
_Avoid_: phase, job (job is overloaded; "stage" is the unit). "Step" names the
config-level unit, which is wider: a PR loop has Steps that are not Stages.

**Key**:
The tracker-agnostic ticket identifier (`BEH-123`, `PROJ-123`, `#123`) the harness
slugs branches from and greps history with. The harness treats it as an opaque
string supplied by the Tracker adapter.
_Avoid_: ticket id, BEH number, issue number.

**Lease**:
One run's claim on one **Key** (`internal/lease`). It knows whether the ticket is
held *right now*: selection hands the run a held lease (ADR-0003), a hand-passed
run starts unheld and claims after preflight. A run settles its lease once —
released to Todo, closed as a no-op, or kept by a shipped PR — and releasing an
unheld lease touches nothing.
_Avoid_: claim flag, pre-claimed.

### The loop

**Source**:
Where a loop's work comes from and where its claims are kept (`loop.Source`): the
queue it polls, the claims the reaper sweeps, the breadcrumbs it leaves. The
tracker's ready queue is the first Source (ADR-0019).
_Avoid_: queue, feed (a queue is one kind of Source).

**Work item**:
One unit of work a **Source** yields (`loop.WorkItem`): a **Key** and the branch
the **Pipeline** will push. For a ticket, the Key is the tracker Key and the
branch is `<branch_prefix>/<slug>`. For an open PR (the **PR loop**), the Key is
`PR-<number>` and the branch is the PR's head branch.
_Avoid_: job, task, ticket (a ticket is one kind of Work item).

**Pipeline** (of a loop):
What a loop does to one **Work item** (`loop.Pipeline`): its **Steps**, in order.
For a ticket, the three **Stages**; for a PR, one fix (a conflict resolution or a
CI fix), then the **Gates**, a push to the existing branch, and the CI watch. The
daemon branches only on the **Disposition** it returns.

**Step**:
One named unit of a loop's **Pipeline**, from a fixed set the harness implements
in Go (`config.Step`): `implement`, `review`, `retro` for a tracker **Source**;
`checkout_pr`, `fix`, `gates`, `push_existing`, `watch_ci`, `retro` for open PRs.
Every Step owns its **Ground truth** check. A Consumer's `[[loops]]` config
chooses, orders and parameterises Steps and supplies their **Project bodies**, but
cannot add one (ADR-0019).
_Avoid_: action, task, hook (a hook is `post_create`, which a Step may run).

**Loop** (named):
A **Source**, its **Steps** and its own limits, run by one daemon under a name
(`agent-harness loop <name>`): the built-in `tickets` (the default) and
`pr-doctor`, or a `[[loops]]` entry in `config.toml`. Each has its own STOP
sentinel (`STOP-<name>`) and its own `loop.jsonl`.

**Instance** (of a loop):
One of several daemons running the same ticket loop against one Consumer
(`--instance <id>`; ADR-0024). Each has its own pidfile, log, `STOP-<id>` and
`logs/instances/<id>/`; the one with no id is the default instance. Instances
share the claim lock, so two never hold one **Lease**, and the gate slots, so only
`GATE_CONCURRENCY` runs gate at once.
_Avoid_: worker, replica, second loop (a second *loop* is a different Source and
Pipeline; an instance is the same loop again).

**PR loop**:
A loop whose **Source** is `github_prs` (ADR-0019); the built-in one is
`pr-doctor`. The Source is the open PRs that conflict with `main` or have red CI
and that opted in: on the harness's `branch_prefix`, or carrying the opt-in label.
Its **Lease** is the `in-progress` label on the PR, keyed on the branch.
_Avoid_: babysitter. ("pr-doctor" is the built-in loop's name, not the concept.)

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
**PreflightAborted**, **Interrupted** or **NoPR** (`stages.Disposition`). Exactly
one holds per stage and, folded across the three, per ticket; it is the only thing
the loop branches on and the breaker counts. Distinct from a Stage's **OK**, which
asks only "did this stage do its job?" — a Shipped run can be not-OK (CI red after
the auto-fix budget) and an OK implementation is still NoPR (it never pushes).
**Interrupted** means a stop signal killed a session from outside (exit 128+N): the
run was cut short, not broken, so it is never called a failure (ADR-0030).
_Avoid_: outcome, signal, status.

**Landing**:
Taking a clean, committed branch to an open PR on the latest base (`ship.Land`):
refetch, replay, disjoint guard, conflict hook, collapse check, push, PR. It ends in
one verdict: **Landed**, **Collapsed**, **Conflict**, **Disjoint**, **PushFailed**
or **PRFailed**. Each caller maps that verdict onto its own **Disposition**.
_Avoid_: ship step, finish, push-and-PR.

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
  the single-role unit; the three together are the ticket loop's **pipeline**.
- "step" was avoided as a synonym for **Stage**. Resolved (ADR-0019): a **Step** is
  the config-level unit of any loop's Pipeline; the three Stages are the ticket
  loop's Steps, and a PR loop has Steps that are not Stages.
- "Linear" was used as a synonym for the issue tracker. Resolved: Linear is one
  **Tracker** adapter; the concept is **Tracker** (ADR-0010).
