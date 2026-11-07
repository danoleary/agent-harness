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
4. If the ticket describes a concrete incident or reproducing scenario (specific
   events, timestamps, IDs, inputs), plan a test that replays that scenario's
   actual data — not a tidied-up fixture — and asserts the incident no longer
   happens. It is in addition to any test the ticket names, and it is your first
   Red test. If the ticket lists several candidate fixes, check the one you chose
   against that replayed scenario before you commit: a fix that passes the
   requested test can still leave one of the incident's own data points on the
   wrong side of the boundary it relies on. Arguing in your summary that the fix
   covers the scenario is not a substitute for the test.
5. Read the repo guards below. `make check` rejects code that breaks them, and
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
- **A `scripts/test-*.sh` that runs `git init` or `git clone` must source
  `scripts/lib/scratch-git.sh`** (and build the repo with `scratch_git_repo`) —
  `check-script-git` (`scripts/check-script-test-git-config.sh`). The helper
  cuts git off from the operator's global config, which may sign commits; the
  sandbox has none, so nothing else sees the difference (#166).
- **Every `go build`/`test`/`run`/`install` in a Makefile keeps
  `-buildvcs=false`**, on one line — `check-buildvcs` (`scripts/check-buildvcs.sh`).
  Without it go aborts inside a linked worktree.
- **Two ADRs can't share a number, and every `ADR-NNNN` reference (Go comment or
  Markdown) or `adr/NNNN-<slug>.md` link must name an existing file, and
  every ADR has a line in the index, `docs/adr/README.md`, in ascending
  order** — `check-adr` (`scripts/check-adr-numbers.sh`); see Finish.
- **Every relative link in a `.md` on `install.sh`'s `DOCS` list (the README,
  `docs/DESIGN.md`, …), resolved from that doc's own directory, must name a
  file on that list** (not a directory, not a path outside the bundle), and
  every doc on that list must exist — `check-bundle-links`
  (`scripts/check-bundle-links.sh`). The installed docs sit beside only each
  other; link anything else, an ADR included, by its `https://github.com/...`
  URL or add it to `DOCS`.
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

Commit before the full gate, not after it. A session that runs out of time
inside `make check` with nothing committed leaves no handoff commit, and the
harness has to spend a whole extra cycle recovering the diff (#224).

1. Update docs the change makes stale (README, `docs/DESIGN.md`, an ADR).
   Before numbering a new ADR, check `origin/main`'s `docs/adr/` (`git fetch`
   first), not local main, and expect a sibling branch may take the same number.
   Append the new ADR's line to the end of `docs/adr/README.md`: a sibling that
   took the same number then conflicts with yours there (ADR-0034), and
   `make check` (`scripts/check-adr-numbers.sh`) fails on a collision as the
   backstop. That fetch is read-only: do not rebase or merge `origin/main` into
   the branch, even if main has moved. The harness rebases it before push and
   resolves conflicts in its own session.
2. Run `go test` and `go vet` on the packages the diff touches, then
   `gofmt -l .`, each as its own Bash call with nothing appended. All must be
   clean.
3. Make ONE commit on the branch the prompt names. Subject: an imperative
   sentence describing the behaviour, ending with the ticket reference, e.g.
   `Give the ticket claim one owner: a per-run lease (#25)`. The body says why.
4. Run `make check` as its own Bash call with nothing appended. Fold any fix it
   needs into that commit with `git commit --amend`, never a second commit.
5. If `make check` fails only in a package the diff didn't touch, re-run that
   package alone: `go test -count=1 ./internal/<pkg>/`. If it passes, note it
   in your summary as a suspected load flake and stop: do not re-run the full
   `make check`. It can take as long as the first run, and the harness re-gates
   host-side after the session anyway. If it fails alone too, treat it as real.
