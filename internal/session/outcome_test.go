package session

import (
	"testing"

	"github.com/beherd/agent-harness/internal/sandbox"
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
		{"transient 125 read-only", Outcome{ExitCode: sandbox.ExitCannotStart, DockerReason: `failed to remove root filesystem: unlinkat /var/lib/docker/overlay2/x: read-only file system`}, true},
		{"transient 125 unexpected EOF", Outcome{ExitCode: sandbox.ExitCannotStart, DockerReason: `level=error msg="error waiting for container: unexpected EOF"`}, true},
		{"genuine 125 daemon down", Outcome{ExitCode: sandbox.ExitCannotStart, DockerReason: "Cannot connect to the Docker daemon"}, false},
		{"125 with no reason", Outcome{ExitCode: sandbox.ExitCannotStart}, false},
		{"success", Outcome{ExitCode: 0}, false},
		{"real failure", Outcome{ExitCode: 1}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.o.Retryable(); got != c.want {
				t.Errorf("Outcome{%d, %q}.Retryable() = %v, want %v", c.o.ExitCode, c.o.DockerReason, got, c.want)
			}
		})
	}
}
