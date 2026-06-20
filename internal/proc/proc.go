// Package proc runs external commands under a wall-clock deadline, so a wedged
// dependency — a hung Docker daemon, a stalled git remote — fails fast with a
// clear error instead of blocking the harness forever. A daemon that went
// read-only once hung an implementation run for two days because every docker
// call used an unbounded exec.Command (BEH-386).
package proc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrTimeout wraps any command killed for exceeding its deadline, so callers can
// tell a hung dependency apart from an ordinary non-zero exit.
var ErrTimeout = errors.New("command timed out")

// CombinedOutput runs name+args under timeout, returning combined stdout+stderr.
// If the command outlives the deadline it is killed and the returned error wraps
// ErrTimeout. A non-positive timeout means no deadline.
func CombinedOutput(timeout time.Duration, name string, args ...string) ([]byte, error) {
	return CombinedOutputInDir(timeout, "", name, args...)
}

// CombinedOutputInDir is CombinedOutput with the command's working directory set
// to dir (an empty dir means the caller's cwd, matching exec.Cmd). Used for
// remote ops that infer their target from the checkout they run in — e.g.
// `gh pr create` reads the origin repo from herdPath — which must still be bounded
// so a stalled network or blocking auth fails fast rather than hanging (BEH-386).
func CombinedOutputInDir(timeout time.Duration, dir, name string, args ...string) ([]byte, error) {
	ctx, cancel := withTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return out, wrapTimeout(ctx, name, timeout, err)
}

// Output runs name+args under timeout, returning stdout and stderr as separate
// buffers plus the exit error. Unlike CombinedOutput it keeps the two streams
// apart so a tool that writes machine-readable output to stdout AND exits
// non-zero (notably `gh pr checks --json`, which exits non-zero when checks fail
// but still prints the JSON) can be parsed: the caller reads stdout regardless
// and only falls back to the error + stderr when stdout won't parse. The error
// carries stderr's last line on an ordinary failure and wraps ErrTimeout on a
// deadline kill, same as Run.
func Output(timeout time.Duration, name string, args ...string) (stdout, stderr []byte, err error) {
	return OutputInDir(timeout, "", name, args...)
}

// OutputInDir is Output with the command's working directory set to dir (empty
// means the caller's cwd). Used for the host-side `gh` calls that infer their
// target repo from the checkout they run in — `gh pr checks`, `gh run view
// --log-failed`, `gh run rerun` all read the herd checkout, same as createPR.
func OutputInDir(timeout time.Duration, dir, name string, args ...string) (stdout, stderr []byte, err error) {
	ctx, cancel := withTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	runErr := wrapStderr(wrapTimeout(ctx, name, timeout, cmd.Run()), errBuf.Bytes())
	return out.Bytes(), errBuf.Bytes(), runErr
}

// Run runs name+args under timeout. On the happy path it stays quiet (stdout is
// discarded, nil error); on a non-zero exit it captures stderr and folds its last
// line into the returned error, so a git failure carries the actual `fatal: …`
// cause instead of a bare `exit status 128` (BEH-404). Same deadline semantics as
// CombinedOutput — used for git remote ops, where only success/failure matters.
func Run(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := withTimeout(timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return wrapStderr(wrapTimeout(ctx, name, timeout, err), stderr.Bytes())
}

func withTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), timeout)
}

// wrapTimeout converts a deadline kill into an ErrTimeout-wrapped error; any
// other failure (or success) passes through unchanged.
func wrapTimeout(ctx context.Context, name string, timeout time.Duration, err error) error {
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%q did not respond within %s: %w", name, timeout, ErrTimeout)
	}
	return err
}

// wrapStderr appends stderr's last non-empty line to an ordinary command failure,
// turning git's opaque `exit status 128` into the diagnostic it printed. Successes
// (nil err) and deadline kills (already an ErrTimeout — keep its message intact)
// pass through untouched, so the happy path stays quiet.
func wrapStderr(err error, stderr []byte) error {
	if err == nil || errors.Is(err, ErrTimeout) {
		return err
	}
	if line := lastNonEmptyLine(stderr); line != "" {
		return fmt.Errorf("%w: %s", err, line)
	}
	return err
}

// lastNonEmptyLine returns the final non-blank line of b, trimmed. git's fatal
// cause is the last line it writes, so this is the one worth surfacing. Splits on
// both \n and \r so a fatal that rides on a bare-\r progress segment (git
// overwrites progress with \r, no \n) surfaces clean, without embedded carriage
// returns or progress noise prefixed.
func lastNonEmptyLine(b []byte) string {
	lines := strings.FieldsFunc(string(b), func(r rune) bool {
		return r == '\n' || r == '\r'
	})
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
