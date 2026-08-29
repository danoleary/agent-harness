package session

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/sandbox"
)

// Retryable folds the two environmental, transient session failures into one
// predicate the retry loop keys off: the 137 OOM-kill and the transient exit-125
// launch failure (overlay2/read-only-fs, BEH-542). A genuine 125 (daemon down,
// image missing) and any real non-zero code stay terminal.
func TestOutcomeRetryable(t *testing.T) {
	cases := []struct {
		name string
		o    Outcome
		want bool
	}{
		{"oom 137", Outcome{ExitCode: sandbox.ExitOOMKill}, true},
		// A watchdog cap-kill also exits 137 (docker kill → SIGKILL), but it ran the
		// full session doing real work — it is NOT a bare pre-work transient, so it
		// must not be retried from scratch (BEH-668).
		{"cap-kill 137", Outcome{ExitCode: sandbox.ExitOOMKill, CapKilled: true}, false},
		{"transient 125 read-only", Outcome{ExitCode: sandbox.ExitCannotStart, DockerReason: `failed to remove root filesystem: unlinkat /var/lib/docker/overlay2/x: read-only file system`}, true},
		{"transient 125 unexpected EOF", Outcome{ExitCode: sandbox.ExitCannotStart, DockerReason: `level=error msg="error waiting for container: unexpected EOF"`}, true},
		{"genuine 125 daemon down", Outcome{ExitCode: sandbox.ExitCannotStart, DockerReason: "Cannot connect to the Docker daemon"}, false},
		{"125 with no reason", Outcome{ExitCode: sandbox.ExitCannotStart}, false},
		{"success", Outcome{ExitCode: 0}, false},
		{"real failure", Outcome{ExitCode: 1}, false},
		// A deterministic zero-work crash (a prompt-expansion no-op that billed $0 —
		// BEH-691) is never a transient worth retrying: re-launching the identical
		// prompt fails identically. Defensive even against an OOM exit code — a
		// $0-cost clean result and a 137 SIGKILL cannot co-occur, but the flag wins.
		{"no real turns", Outcome{ExitCode: 0, NoRealTurns: true}, false},
		{"no real turns over a 137", Outcome{ExitCode: sandbox.ExitOOMKill, NoRealTurns: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.o.Retryable(); got != c.want {
				t.Errorf("Outcome{%d, %q}.Retryable() = %v, want %v", c.o.ExitCode, c.o.DockerReason, got, c.want)
			}
		})
	}
}
