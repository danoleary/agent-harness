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
4. Read the repo guards below. `make check` rejects code that breaks them, and
   finding that out after the code is written costs a rewrite and a full re-run.

## Repo guards `make check` enforces

- **No raw `exec.Command` for docker, `gh` or a git remote op** (`fetch`, `push`,
  `clone`, `ls-remote`, `pull`) outside `internal/proc` — `check-exec`
  (`scripts/check-exec-command.sh`). Call `proc.Run`, `proc.Output`,
  `proc.CombinedOutput` and their `InDir`/`WithInput` variants with a timeout, so a
  hung daemon or remote fails fast instead of wedging the run (BEH-386). If the
  variant you need (stdin, dir, env, …) is missing, add it to `internal/proc` with
  a test rather than bypassing it. A deliberately unbounded call carries an
  inline `// allow-unbounded-exec: <reason>` on the call line.
- **Every bash-shebang script (`scripts/*.sh` and any other) must run under
  macOS bash 3.2** — `check-bash3` (`scripts/check-bash3-compat.sh`): no
  `declare -A`, namerefs (`declare -n`), `${var,,}`/`${var^^}` or
  `mapfile`/`readarray`.
- **awk in `scripts/*.sh` and `install.sh` must run under mawk (the sandbox),
  BWK awk (macOS) and gawk (CI)** — `check-awk` (`scripts/check-awk-portability.sh`):
  no regex interval expressions (`{n}`, `{n,m}`; mawk silently never matches them,
  so repeat the atom or use `?`) and no `gensub`, `asort`/`asorti` or `strftime`.
- **Every `go build`/`test`/`run`/`install` in a Makefile keeps
  `-buildvcs=false`**, on one line — `check-buildvcs` (`scripts/check-buildvcs.sh`).
  Without it go aborts inside a linked worktree.
- **Two ADRs can't share a number, and every `ADR-NNNN` reference (Go comment or
  Markdown) or `adr/NNNN-<slug>.md` link must name an existing file** —
  `check-adr` (`scripts/check-adr-numbers.sh`); see Finish.
- **Every relative link in `README.md` must name a file on `install.sh`'s
  `DOCS` list** (not a directory, not a path outside the bundle), and every doc
  on that list must exist — `check-bundle-links`
  (`scripts/check-bundle-links.sh`). The installed README sits beside only
  those docs; link anything else by its `https://github.com/...` URL or add it
  to `DOCS`.
- **CI runs `make check` itself, never its sub-targets one by one** — `check-ci`
  (`scripts/check-ci-runs-check.sh`); wire a new guard into `check:` only, and
  add a line for it to this list (`internal/docs` fails the gate otherwise).
- A changed `scripts/<name>.sh` that has a sibling `scripts/test-<name>.sh` must
  change that test too (CI checks this), and `check-scripts` runs every
  `scripts/test-*.sh`.
- `example/.agent-harness/` is loaded by the test suite; keep it valid if you
  change the config schema, and update `docs/reference/config.md` alongside it.
- A new env setting goes in `internal/config/config.go`,
  `docs/reference/env.md` and `.env.example`. The scaffold copy,
  `internal/scaffold/templates/env.example`, is drift-checked against it.

## The loop

Repeat per behaviour, smallest first:

1. **Red.** Write one test that states the behaviour through the module's public
   interface. Run `go test ./internal/<pkg>/ -run <TestName>` and see it fail
   for the reason you expect.
2. **Green.** Write the least code that passes it.
3. **Refactor.** Remove duplication you just created; keep the tests green.

Test behaviour, not implementation: a test that has to change when you rename a
private helper is testing the wrong thing.

## Finish

1. Run `gofmt -l .` and then `make check`, each as its own Bash call with
   nothing appended. Both must be clean.
2. Update docs the change makes stale (README, `docs/DESIGN.md`, an ADR).
   Before numbering a new ADR, check `origin/main`'s `docs/adr/` (`git fetch`
   first), not local main, and expect a sibling branch may take the same number;
   `make check` (`scripts/check-adr-numbers.sh`) fails on a collision as the
   backstop. That fetch is read-only: do not rebase or merge `origin/main` into
   the branch, even if main has moved. The harness rebases it before push and
   resolves conflicts in its own session.
3. Make ONE commit on the branch the prompt names. Subject: an imperative
   sentence describing the behaviour, ending with the ticket reference, e.g.
   `Give the ticket claim one owner: a per-run lease (#25)`. The body says why.
