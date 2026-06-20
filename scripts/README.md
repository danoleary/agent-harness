# Agent Harness Scripts

## check-buildvcs.sh

Guard against VCS-stamping Go commands in Makefiles that omit `-buildvcs=false`
(BEH-459).

### Purpose

The Go toolchain stamps VCS metadata into `main` packages and shells out to git
to do it. Inside a linked git worktree (the `.claude/worktrees/<slug>` path the
tdd/review skills create), that git call returns `exit status 128`, so
`go build`/`go test` abort **before running anything**:

```
error obtaining VCS status: exit status 128
        Use -buildvcs=false to disable VCS stamping.
```

`go vet` is unaffected (it doesn't stamp), which makes the failure look
inconsistent and easy to misread as a code problem. Appending `-buildvcs=false`
disables the stamping; it is a **no-op in a normal checkout** (CI), so it only
unblocks worktree work. This guard fails the gate if a VCS-stamping go command
appears in a Makefile recipe without the flag, so the fix can't silently
regress. It is itself bash-3.2-safe.

### What it checks

Scans every `Makefile` / `*.mk` / `*.make` under the target dir and flags, in
non-comment lines, any `go build` / `go install` / `go run` / `go test`
invocation that lacks `-buildvcs=false`. Non-stamping go subcommands (`go vet`,
`go mod tidy`, `gofmt`, `go version`) and `docker build` are not flagged.

### Usage

```bash
# Check the whole harness (the make check-buildvcs default)
./scripts/check-buildvcs.sh

# Check a specific directory
./scripts/check-buildvcs.sh /path/to/dir
```

### Testing

```bash
./scripts/test-check-buildvcs.sh
```

## check-bash3-compat.sh

Guard against bash-4-only syntax in bash scripts (BEH-457).

### Purpose

Harness scripts run on developers' default shell — on macOS, the stock **bash
3.2**. A bash-4-only construct under a `#!/bin/bash` shebang dies there with an
opaque `unbound variable` and the script **silently never runs** (how
`check-exec-command.sh` became a no-op before BEH-409). This guard fails the
gate on those constructs so they can't ship. It is itself bash-3.2-safe.

### What it checks

Scans every regular file whose first line is a bash shebang (`#!/bin/bash` or
`#!/usr/bin/env bash`) and flags, outside full-line comments:

- `declare -A` / `local -A` / `typeset -A` — associative arrays (bash 4.0+)
- `declare -n` / `local -n` / `typeset -n` — namerefs (bash 4.3+)
- `${var,,}` / `${var^^}` / `${var,}` / `${var^}` — case modification (bash 4.0+)
- `mapfile` / `readarray` — bash 4.0+

Non-bash files (`#!/bin/sh`, plain text), full-line comments, and the guard's
own files are not flagged. Indirect/array-index expansions like `${!arr[@]}` are
deliberately **not** flagged — they are valid on bash 3.2 indexed arrays and
would false-positive.

### Usage

```bash
# Check the entire harness (the make check-bash3 default)
./scripts/check-bash3-compat.sh

# Check a specific directory
./scripts/check-bash3-compat.sh /path/to/code
```

### Testing

```bash
./scripts/test-check-bash3-compat.sh
```

## check-exec-command.sh

Guard against unbounded `exec.Command` calls to daemon/remote tools (BEH-388).

### Purpose

Prevents re-introducing the "wedged dependency hangs a run forever" bug by detecting raw `exec.Command`/`exec.CommandContext` calls to external daemons and remote services that lack timeouts.

### What it checks

**Requires timeouts (violations):**
- Docker daemon operations (`docker` commands)
- GitHub CLI remote calls (`gh` commands)
- Git remote operations (`fetch`, `push`, `clone`, `ls-remote`, `pull`)

**Explicitly allowed:**
- Local git operations (`status`, `log`, `rev-list`, `rev-parse`, `worktree`) - deliberately deferred per BEH-386
- Non-daemon commands (`ls`, `echo`, `cat`, etc.)
- Anything in `internal/proc/` (the bounded execution package)

**Allowlisted exceptions:**
- `internal/session/session.go:55` - Main docker run with timer-based kill
- `internal/session/session.go:75` - Docker kill called by timeout handler
- `internal/sandbox/sandbox.go:192` - Docker build with visible progress

### Usage

```bash
# Check the entire harness
./scripts/check-exec-command.sh

# Check a specific directory
./scripts/check-exec-command.sh /path/to/code
```

### Integration

The guard is integrated into the Makefile's `check` target, which runs:
1. `fmt-check` - Go format verification
2. `vet` - Go vet static analysis
3. `check-exec` - This exec.Command guard
4. `check-bash3` - The bash 3.2 compatibility guard
5. `check-buildvcs` - The -buildvcs=false guard (BEH-459)
6. `check-ci` - The CI-runs-check guard (BEH-462)
7. `test` - Full test suite

CI runs `make check` directly (then `make build`), so adding a guard to the
`check` target above covers it in CI with no workflow edit. The `check-ci` guard
enforces that — see below.

### Testing

Run the test suite to verify the guard works correctly:

```bash
./scripts/test-check-exec-command.sh
```

### Fix violations

When the guard reports a violation, fix it by using `internal/proc` with an appropriate timeout:

```go
// Instead of:
exec.Command("docker", "info").Run()

// Use:
proc.Run(30*time.Second, "docker", "info")
```

See `internal/proc/proc.go` for available functions.

## check-ci-runs-check.sh

Guard against the harness CI workflow drifting from the Makefile `check` target
(BEH-462).

### Purpose

The CI workflow (`.github/workflows/agent-harness.yaml`) used to run each `make
check` sub-target as its own step (`make fmt-check`, `make vet`, `make
check-exec`, …). That step list is a hand-maintained copy of the `check` target's
prerequisites, and the two silently diverge: a guard wired into `check:` but
never added to the YAML never runs in CI — exactly the CI-invisible-guard bug
behind BEH-457/BEH-409 (`check-exec` was a no-op in CI for an unknown number of
sessions). CI now runs `make check` directly (then `make build`), so the `check`
target is the single source of truth and a new sub-target is covered for free.
This guard enforces that arrangement so it can't regress, and is itself
bash-3.2-safe.

### What it checks

Reads the `check` target's prerequisites straight from the Makefile, then scans
the workflow's `make <target>` invocations (skipping comment lines) and fails if:

- any `check` prerequisite is run as its own step (re-enumeration drift), or
- the workflow never invokes `make check` at all.

Because the prerequisite list is read from the Makefile, a sub-target added to
`check:` is covered with no edit to this guard.

### Usage

```bash
# Check the real workflow + Makefile (the make check-ci default)
./scripts/check-ci-runs-check.sh

# Check explicit files (used by the tests)
./scripts/check-ci-runs-check.sh path/to/workflow.yaml path/to/Makefile
```

### Testing

```bash
./scripts/test-check-ci-runs-check.sh
```
