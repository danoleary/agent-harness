# ADR-0072: A diff-scoped gate is compared on the base over the branch's paths, or not at all

- Status: Accepted
- Date: 2026-11-24
- Amends: ADR-0048 (what the red-base check runs over origin/main alone)

## Context

ADR-0048 has a conflict resolution whose re-gate is red gate origin/main
alone, with the same gate commands, to tell a red base from a red branch. herd's
Storybook and unit gates run only what the diff against origin/main reaches
(`--changed origin/main`). On a checkout of origin/main that diff is empty. So
each printed `no story reaches the diff; 0 stories ran` and exited 0. The base
was declared green and the branch red, though the failing story was never run
on the base, and it existed there already. The check spent a `post_create` and
a gate slot to learn nothing, on every red re-gate of the gates that most often
go red (#448).

No checkout of the base can make that diff non-empty: the diff compares the
tree with origin/main, and the base tree is origin/main. The gate has to be
told its targets, and only the Consumer knows how its runner takes them.

## Decision

**A gate scoped by the diff against origin/main is compared on the base with
its `base_command`, given the branch's changed paths. Without one, the check is
inconclusive, and a base run that ran zero tests is inconclusive too. Neither
counts as a green base.**

- **Diff-scoped.** A gate is diff-scoped when its command names `origin/main`
  or it sets `base_command`.
- **`base_command`.** A new optional `[[gates]]` key. Only the red-base check
  runs it, in place of `command`, for the gate the re-gate failed. `{paths}` in
  it becomes the paths of `git diff --name-only origin/main` in the branch's
  worktree, each single-quoted. That is the input the branch's own run
  selected from. Every other gate in the base run keeps its `command`.
- **Inconclusive, before gating.** A red diff-scoped gate with no
  `base_command`, or a branch diff that is empty or unreadable, gates nothing on
  the base and records no verdict. The log says `base check inconclusive: 0
  targets selected`, and the red stays the branch's.
- **A recorded green does not answer.** A green verdict for the base (the
  ticket loop records one per SHA) was gated with an empty diff, so it says
  nothing of a diff-scoped gate. A gate with a `base_command` is compared
  anyway. A recorded red on the same gate still answers, as before.
- **Inconclusive, after gating.** A gate can be diff-scoped behind a wrapper
  (`make test-changed`). When the base run of the branch's red gate passes but
  its log reports it ran zero tests (`0 tests ran`, `no story reaches the
  diff`, `Ran 0 tests`), the log says the check is inconclusive. The red stays
  the branch's.

## Alternatives considered

- **Parse the failing test files from the branch's gate log and run those.**
  The harness still could not pass them to a runner whose syntax it does not
  know, so it needs a Consumer-supplied command either way. The branch's
  changed paths are what its own run selected from, and git gives them exactly.
- **Gate only the failing gate on the base.** That saves the other gate runs,
  but the verdict record is shared with the ticket loop, which reads a recorded
  green as every gate passing. It is left for later.
- **Point the base checkout's `origin/main` elsewhere.** Remote-tracking refs
  are shared by every worktree of the checkout, so that would move it under
  every run at once.

## Consequences

- A Consumer whose diff-scoped gate goes red over a red base gets a held PR,
  not a blamed one, only after adding `base_command`. Until then the outcome
  for the branch is the one it had, but no prep or gate slot is spent on the
  base, and the log no longer implies the base passed.
- A base verdict recorded from a `base_command` run reflects the branch's
  targets. A red one holds every PR on that base until main moves or it
  expires, as any red verdict does.
- The zero-tests patterns are text. A runner that reports an empty run in
  other words is not caught. Its green still leaves the red with the branch,
  which is what happened before.
