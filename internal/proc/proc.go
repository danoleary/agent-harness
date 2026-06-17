// Package proc runs external commands under a wall-clock deadline, so a wedged
// dependency — a hung Docker daemon, a stalled git remote — fails fast with a
// clear error instead of blocking the harness forever. A daemon that went
// read-only once hung an implementation run for two days because every docker
// call used an unbounded exec.Command (BEH-386).
package proc

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
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

// Run runs name+args under timeout, discarding output. Same deadline semantics
// as CombinedOutput — used for git remote ops, where only success/failure matters.
func Run(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := withTimeout(timeout)
	defer cancel()
	err := exec.CommandContext(ctx, name, args...).Run()
	return wrapTimeout(ctx, name, timeout, err)
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
