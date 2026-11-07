package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ship"
)

// The review stage records its last push decision — the tip its host Gate re-run
// covered, how it exited, and the verdict — in the ticket's log dir, which outlives
// the run, and the committed-fix recovery reads it back (#230). These tests replay
// the two herd incidents end to end: the review refuses to ship, and the recovery
// that runs over the very worktree it left must refuse too.

// redGate is a host whose named Gate exits code and whose other containers pass.
func redGate(h *hostio.Fake, name string, code int) {
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, gateStep) && strings.HasSuffix(s.Label, "-"+name) {
			return session.Outcome{ExitCode: code}
		}
		return session.Outcome{}
	}
}

func gatesNamed(names ...string) config.Config {
	cfg := stageCfg()
	cfg.Gates = nil
	for _, n := range names {
		cfg.Gates = append(cfg.Gates, config.Gate{Name: n, Command: "pnpm run " + n})
	}
	return cfg
}

func TestReviewThatRefusedToShipLeavesARecoveryNothingToLand(t *testing.T) {
	const blocked = "Disposition: blocked — the sandbox cannot regenerate database.types.ts, so a human must"
	cases := []struct {
		name    string
		cfg     config.Config
		host    func(h *hostio.Fake)
		want    ship.PushGate
		refused []string
	}{
		// BEH-1305: review wrote Disposition: blocked, and the unit gate was red.
		{"blocked verdict over a red unit gate (BEH-1305)", gatesNamed("unit"), func(h *hostio.Fake) {
			h.AgentOutcome = session.Outcome{ReviewVerdictEmitted: true, ReviewBlocked: true, ReviewDisposition: blocked}
			redGate(h, "unit", 1)
		}, ship.PushGate{Disposition: blocked, Blocked: true, GateRan: true, GateExit: 1, FailedGate: "unit"}, []string{"blocked", `"unit"`}},
		// BEH-1198: review stopped on a red storybook gate.
		{"red storybook gate (BEH-1198)", gatesNamed("unit", "storybook"), func(h *hostio.Fake) {
			h.AgentOutcome = session.Outcome{ReviewVerdictEmitted: true, ReviewDisposition: "Disposition: clear — no findings"}
			redGate(h, "storybook", 1)
		}, ship.PushGate{Disposition: "Disposition: clear — no findings", GateRan: true, GateExit: 1, FailedGate: "storybook"}, []string{`"storybook"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := handedOff(t, "PROJ-8")
			tc.host(h)
			log := stageLog(t, "PROJ-8")

			if res := Review(h, tc.cfg, log, Args{Identifier: "PROJ-8"}); res.Disposition == Shipped {
				t.Fatalf("the review must refuse to ship, got %+v", res)
			}

			got := LastPushGate(log.Dir)
			tip, _ := h.Worktree().HeadSHA()
			tc.want.Head = tip
			if got == nil || *got != tc.want {
				t.Fatalf("LastPushGate = %+v, want %+v", got, tc.want)
			}

			// The no-PR run reaches the daemon, which tries the committed-fix recovery
			// over the clean, committed worktree the review left.
			h.OpenPRThere = false
			rec := ship.Recover(h, log, "PROJ-8", got)
			if !rec.Attempted || rec.Shipped {
				t.Fatalf("Recover() = %+v, want an attempted recovery that refused", rec)
			}
			for _, want := range tc.refused {
				if !strings.Contains(rec.Refused, want) {
					t.Errorf("Refused = %q, want it to name %q", rec.Refused, want)
				}
			}
			assertNothingPushed(t, h, repo)
			if called(h, "create-pr PROJ-8: a ticket") {
				t.Errorf("calls = %v, want no PR for a branch the review refused", h.Calls)
			}
		})
	}
}

// BEH-713, the case the recovery exists for, still lands: a review that stopped
// short of its verdict over a clean worktree whose Gates were green records a green
// push gate with no verdict, and the recovery finishes the branch.
func TestReviewWithNoVerdictOverGreenGatesLeavesARecoveryABranchToLand(t *testing.T) {
	h, _ := handedOff(t, "PROJ-9")
	h.AgentOutcome = session.Outcome{} // never emits a verdict
	log := stageLog(t, "PROJ-9")

	if res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-9"}); res.OK {
		t.Fatalf("an unreviewed diff fails the push closed, got %+v", res)
	}
	got := LastPushGate(log.Dir)
	tip, _ := h.Worktree().HeadSHA()
	if want := (ship.PushGate{Head: tip, GateRan: true}); got == nil || *got != want {
		t.Fatalf("LastPushGate = %+v, want %+v", got, want)
	}

	h.OpenPRThere = false
	if rec := ship.Recover(h, log, "PROJ-9", got); !rec.Shipped {
		t.Fatalf("Recover() = %+v, want the gate-green branch shipped (BEH-713)", rec)
	}
}

// A run that emits no verdict over the tip an earlier run's verdict covered keeps
// that verdict: the earlier clear is still handed to a re-review (#44), and an
// earlier blocked verdict still refuses the recovery.
func TestPushGateKeepsTheVerdictOfTheSameTip(t *testing.T) {
	dir := t.TempDir()
	if err := recordPushGate(dir, ship.PushGate{Head: "abc", Disposition: "Disposition: blocked — x", Blocked: true, GateRan: true}); err != nil {
		t.Fatal(err)
	}
	if err := recordPushGate(dir, ship.PushGate{Head: "abc", GateRan: true, GateExit: 0}); err != nil {
		t.Fatal(err)
	}
	if got := LastPushGate(dir); got == nil || !got.Blocked || got.Disposition != "Disposition: blocked — x" {
		t.Errorf("LastPushGate = %+v, want the blocked verdict kept over the same tip", got)
	}
	if err := recordPushGate(dir, ship.PushGate{Head: "def", GateRan: true}); err != nil {
		t.Fatal(err)
	}
	if got := LastPushGate(dir); got == nil || got.Blocked || got.Disposition != "" {
		t.Errorf("LastPushGate = %+v, want a new tip to drop the old verdict", got)
	}
}

// A record written before the push gate was recorded carries only the verdict, and
// reads back with GateRan=false — which the recovery declines — rather than as a
// green gate (its zero exit code).
func TestPushGateFromAnOlderRecordHasNoGate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, priorReviewFile), []byte(`{"head":"abc","disposition":"Disposition: clear — ok"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LastPushGate(dir); got == nil || got.GateRan {
		t.Errorf("LastPushGate = %+v, want the verdict with no gate run", got)
	}
	if got := LastPushGate(t.TempDir()); got != nil {
		t.Errorf("LastPushGate over an empty dir = %+v, want nil", got)
	}
}
