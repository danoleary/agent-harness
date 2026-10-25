package session

import (
	"os/exec"
	"syscall"
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

// A session killed from outside by a stop signal exits 128+N (#99). That is an
// interruption — the run was cut short, not broken — so it is classified apart
// from a real failure and from the retryable crashes. 137 (SIGKILL) is not one:
// it is the OOM-kill and the watchdog's own kill, which have their own classes.
func TestOutcomeInterrupted(t *testing.T) {
	cases := []struct {
		name string
		code int
		want syscall.Signal
		ok   bool
	}{
		{"SIGTERM 143", 143, syscall.SIGTERM, true},
		{"SIGINT 130", 130, syscall.SIGINT, true},
		{"SIGHUP 129", 129, syscall.SIGHUP, true},
		{"SIGQUIT 131", 131, syscall.SIGQUIT, true},
		{"SIGKILL 137 is the OOM/watchdog class", sandbox.ExitOOMKill, 0, false},
		{"SIGSEGV 139 is a crash", 139, 0, false},
		{"success", 0, 0, false},
		{"real failure", 1, 0, false},
		{"cannot start", sandbox.ExitCannotStart, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := Outcome{ExitCode: c.code}
			sig, ok := o.Interrupted()
			if ok != c.ok || sig != c.want {
				t.Errorf("Outcome{%d}.Interrupted() = (%v, %v), want (%v, %v)", c.code, sig, ok, c.want, c.ok)
			}
			if ok && o.Retryable() {
				t.Errorf("Outcome{%d} is both interrupted and retryable", c.code)
			}
		})
	}
}

// When the docker client itself is the process the signal killed, it has no exit
// code of its own (-1); the session reports it the way a shell would, as 128+N, so
// it is classified the same as a container that died of the signal.
func TestExitCodeOfSignalledProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if got := waitExitCode(err); got != 128+int(syscall.SIGTERM) {
		t.Errorf("exitCode(signalled by SIGTERM) = %d, want %d", got, 128+int(syscall.SIGTERM))
	}
	if got := waitExitCode(exec.Command("sh", "-c", "exit 3").Run()); got != 3 {
		t.Errorf("waitExitCode(exit 3) = %d, want 3", got)
	}
	if got := waitExitCode(nil); got != 0 {
		t.Errorf("waitExitCode(nil) = %d, want 0", got)
	}
}
