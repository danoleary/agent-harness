---
name: tdd
description: Implement a ticket test-first in this Go repo (red → green → refactor), ending in one commit that passes `make check`. Use when the harness implementation stage invokes /tdd.
---

# /tdd — implement a ticket test-first

You are implementing one ticket in the agent harness's own Go codebase. The
prompt around this skill tells you the ticket, the worktree and the branch.

## Before writing code

1. Read the ticket. Restate, in two or three sentences, the behaviour that will
   be different when you are done. If the ticket names an issue that already
   looks fixed on `main`, say so and stop: that is a legitimate outcome.
2. Read `CONTEXT.md` and use its terms (Stage, Gate, Lease, Landing,
   Disposition, …) in names, comments and commit messages.
3. Find the module that owns the behaviour and its `_test.go`. Follow the
   existing test style there — table tests, fakes over mocks, the port
   interfaces in `internal/hostio` and friends — rather than inventing a new one.

## The loop

Repeat per behaviour, smallest first:

1. **Red.** Write one test that states the behaviour through the module's public
   interface. Run `go test ./internal/<pkg>/ -run <TestName>` and see it fail
   for the reason you expect.
2. **Green.** Write the least code that passes it.
3. **Refactor.** Remove duplication you just created; keep the tests green.

Test behaviour, not implementation: a test that has to change when you rename a
private helper is testing the wrong thing.

## Repo rules the gate enforces

- `scripts/*.sh` must run under macOS bash 3.2: no `declare -A`, namerefs,
  `${var,,}` or `mapfile`.
- A changed `scripts/<name>.sh` that has a sibling `scripts/test-<name>.sh` must
  change that test too (CI checks this).
- Any go command in the Makefile keeps `-buildvcs=false`.
- `example/.agent-harness/` is loaded by the test suite; keep it valid if you
  change the config schema, and update `docs/CONSUMER.md` alongside it.

## Finish

1. Run `gofmt -l .` and then `make check`, each as its own Bash call with
   nothing appended. Both must be clean.
2. Update docs the change makes stale (README, `docs/DESIGN.md`, an ADR).
3. Make ONE commit on the branch the prompt names. Subject: an imperative
   sentence describing the behaviour, ending with the ticket reference, e.g.
   `Give the ticket claim one owner: a per-run lease (#25)`. The body says why.
