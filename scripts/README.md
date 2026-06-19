# Agent Harness Scripts

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
4. `test` - Full test suite

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