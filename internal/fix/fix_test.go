package fix

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/session"
)

// BEH-571: sessionError maps a finished auto-fix session's outcome to the
// error its ci.Driver.Fix callback returns. A spending-cap abort is its own
// retry-after-reset class (ci.ErrSpendingCapActive), distinct from a generic
// non-zero exit, and takes precedence over the exit code (a cap abort also exits
// non-zero) so a cap-active window isn't mislabelled as a fix-attempt failure.
func TestFixSessionErrorSpendingCapTakesPrecedence(t *testing.T) {
	// Cap aborts also carry a non-zero exit; the cap class must win.
	err := sessionError(session.Outcome{ExitCode: 1, SpendingCapAbort: true}, 1)
	if !errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("err = %v, want ci.ErrSpendingCapActive", err)
	}
}

func TestFixSessionErrorNonZeroExitIsGenericFailure(t *testing.T) {
	err := sessionError(session.Outcome{ExitCode: 2}, 3)
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("a plain non-zero exit must not be the spending-cap class: %v", err)
	}
	if !strings.Contains(err.Error(), "exited 2") || !strings.Contains(err.Error(), "session 3") {
		t.Fatalf("err %q should name the attempt and exit code", err)
	}
}

func TestFixSessionErrorCleanSessionReturnsNil(t *testing.T) {
	if err := sessionError(session.Outcome{ExitCode: 0}, 1); err != nil {
		t.Fatalf("a clean session must return nil, got %v", err)
	}
}

// BEH-634: RunGates iterates the config-declared named gate list host-side,
// running each gate in order and stopping at the first non-green one. When every
// gate passes it runs them all and reports no failing gate; when one fails it
// reports that gate's name and does not run the gates after it (the push gate
// only needs the first red to withhold the push, and skipping the rest saves a
// container launch). The failing gate's name is what flows into the log + CI-fix
// diagnosis, matching how internal/ci names a failing check.
func TestRunGatesAllGreenRunsEveryGateInOrder(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	var ran []string
	res := RunGates(gates, func(g config.Gate) session.Outcome {
		ran = append(ran, g.Name)
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "" {
		t.Errorf("all-green gates must report no failing gate, got %q", res.FailedGate)
	}
	if res.Outcome.ExitCode != 0 {
		t.Errorf("all-green outcome must be exit 0, got %d", res.Outcome.ExitCode)
	}
	if !reflect.DeepEqual(ran, []string{"check", "typecheck"}) {
		t.Errorf("gates must run in config order, ran %v", ran)
	}
}

func TestRunGatesStopsAtFirstFailureAndReportsItsName(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	var ran []string
	res := RunGates(gates, func(g config.Gate) session.Outcome {
		ran = append(ran, g.Name)
		if g.Name == "check" {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "check" {
		t.Errorf("the first red gate must be reported by name, got %q", res.FailedGate)
	}
	if res.Outcome.ExitCode != 2 {
		t.Errorf("the failing gate's outcome (exit 2) must be returned, got %d", res.Outcome.ExitCode)
	}
	if !reflect.DeepEqual(ran, []string{"check"}) {
		t.Errorf("gates after the first failure must not run, ran %v", ran)
	}
}

func TestRunGatesReportsLaterFailingGateByName(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	res := RunGates(gates, func(g config.Gate) session.Outcome {
		if g.Name == "typecheck" {
			return session.Outcome{ExitCode: 1}
		}
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "typecheck" {
		t.Errorf("the failing gate (typecheck) must be reported by name, got %q", res.FailedGate)
	}
}
