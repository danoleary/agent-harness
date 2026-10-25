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
- A Consumer's body is where a skill invocation comes from. The harness declares
  none, so a Stage with no body has nothing to invoke (see the amendment below).

## Amendment, 2026-09-19: a Consumer body is required

The original decision let a Stage run with no body at all: the envelope was the
contract, and an absent body was the Consumer's quality problem.

That degraded silently in the one direction that costs the most. A skill-less
implementation session still starts a container, still claims the ticket, still
exits 0, and still reports a healthy run — it simply produces no worktree and no
commit. The operator learns this an hour later from an empty branch, and nothing
in the run distinguishes it from a genuine no-op ticket.

**`config.LoadPrompts` now fails when any of the three bodies is missing or
blank**, naming every unusable path in one error. The harness still ships no
default skill — the correction is to refuse to run without the Consumer's
declaration, not to supply one. A default would be worse: the harness cannot know
which skills a project carries, so a baked-in `/tdd` would name a skill that may
not exist there, and the same silent no-op would return wearing a harness name.

## Amendment, 2026-10-24: the review envelope carries a prior verdict on a re-run

A re-run whose implementation stage added no commits handed the review stage the
same cold envelope as a first pass. The reviewer re-derived a verdict that already
existed and, read literally, "fix what is wrong in a local commit" pushed it to
produce a cosmetic commit, so every re-run stacked another "Review fixes" commit.

**The review stage now records each verdict's Disposition line and the HEAD it
covered, in the ticket's log dir, and when the branch HEAD still matches, the
envelope carries both** (#44). The section tells the session to run the gate and
re-emit that Disposition, committing only for a real defect. It sits in the
envelope, not the body, because only the harness knows the branch has not moved.
Only a clear verdict is carried forward: a blocked one waits on a human answer that
lands on the ticket, not the branch, so re-emitting it would never unblock.
It is additive: a first review, or one over a moved branch, reads exactly as
before. The Consumer body still owns how the review is done; the envelope only
states a fact about the diff.

## Amendment, 2026-10-25: the review and retrospective envelopes name the branch base

The review skill, the retrospective skill and the retro body told the session to
read `main...HEAD`. The harness cuts and rebases the branch on origin/main but
never moves local `main`, so once a sibling ticket merged mid-run that range also
carried the sibling's commits. Sessions spent turns working out that the foreign
work was not the branch's, and a less careful reviewer could flag scope creep that
is not there.

**The host now resolves the branch's base — `git merge-base origin/main <branch>`,
the commit it was cut from or last rebased onto — and the review and retrospective
envelopes name it, scoping the change to `<base>..HEAD` / `<base>...HEAD` and
warning off local `main`** (#86). It sits in the envelope because only the host
can resolve it before the session starts. A base the host cannot resolve falls back
to `origin/main`, whose three-dot diff finds the same fork point.

## Amendment, 2026-10-25: the review envelope keeps SHAs out of commit messages

The review skill titled its fix commit `Review fixes for <ticket> over <short
sha>`. A Landing then rebases the branch onto origin/main before the push, which
gives every commit on it a new SHA, so the pushed fix commit named a commit that
was not on the branch or in the PR.

**The review envelope now says that the harness rebases before it pushes and that a
commit message must name the commit it amends by its subject line, not its SHA**
(#95). It sits in the envelope, not just this repo's skill, because the rebase is
the harness's own step: any Consumer's review skill could name a SHA and hit the
same dangling reference. This repo's review skill now titles the commit by what it
fixes instead.
