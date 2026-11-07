package stages

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// baseGateLabel is the container the base check runs the stageCfg "check" Gate in,
// and baseConfirmLabel the one its re-run over a red base runs it in (#189).
const (
	baseGateLabel    = baseGateStep + "-check"
	baseConfirmLabel = baseConfirmStep + "-check"
)

func gateRuns(h *hostio.Fake, label string) int {
	n := 0
	for _, s := range h.Shells {
		if s.Label == label {
			n++
		}
	}
	return n
}

// sharedCfg is stageCfg over a project path the hosts in one test share, so they
// read one another's base verdicts the way loop instances of one Consumer do.
func sharedCfg(t *testing.T) config.Config {
	cfg := stageCfg()
	cfg.ProjectPath = t.TempDir()
	return cfg
}

// redOnBase scripts every base-gate run red and every other shell run green: the
// state of #125, where main itself no longer passes a configured Gate.
func redOnBase(h *hostio.Fake) {
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, baseGateStep) {
			return session.Outcome{ExitCode: 1, StderrTail: "main_test.go:12: undefined: oldHelper"}
		}
		return session.Outcome{}
	}
}

// #125, replayed: two tickets are dispatched off the same base, whose test file no
// longer compiles after two concurrently merged PRs. Before, both sessions found
// the red Gate mid-session and each repaired it in its own branch, so the second
// PR to land conflicted with base. Now neither session launches: the first run
// gates the untouched base, holds its ticket and files one finding for it; the
// second reads that verdict for the same base SHA without gating again.
func TestImplementationHoldsEveryTicketOnARedBase(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)

	before := time.Now()
	first := hostio.NewFake(repo.Checkout(), ticket.Slug("#115"))
	redOnBase(first)
	firstRes := Implementation(first, cfg, stageLog(t, "#115"), Args{Identifier: "#115", Lease: lease.Held(first.Trk, "#115")})

	second := hostio.NewFake(repo.Checkout(), ticket.Slug("#116"))
	redOnBase(second)
	secondRes := Implementation(second, cfg, stageLog(t, "#116"), Args{Identifier: "#116", Lease: lease.Held(second.Trk, "#116")})

	for _, run := range []struct {
		key string
		h   *hostio.Fake
		res Result
	}{{"#115", first, firstRes}, {"#116", second, secondRes}} {
		if run.res.OK || run.res.Disposition != BaseRed {
			t.Errorf("%s: a red base must hold the ticket with the BaseRed disposition, got %+v", run.key, run.res)
		}
		if len(run.h.Agents) != 0 {
			t.Errorf("%s: no session may launch over a red base, got %v", run.key, labels(run.h.Agents))
		}
		if got := run.h.Trk.Released; len(got) != 1 || got[0] != run.key {
			t.Errorf("%s: released = %v, want the held ticket back in Todo", run.key, got)
		}
		if run.h.Worktree().Exists() || run.h.Worktree().BranchExists() {
			t.Errorf("%s: the empty worktree and branch must go, so the next attempt is cut from the new base", run.key)
		}
	}
	// The loop idles on the held base until main moves or the verdict expires
	// (#190), so both holds name the base and the first verdict's expiry: the
	// second read it, it did not restart the clock.
	base := repo.OnOrigin("main")
	if firstRes.HeldBase != base || secondRes.HeldBase != base {
		t.Errorf("held bases = %q / %q, want origin/main %q", firstRes.HeldBase, secondRes.HeldBase, base)
	}
	if until := firstRes.HeldUntil; until.Before(before.Add(baseRedRecheck)) || until.After(time.Now().Add(baseRedRecheck)) {
		t.Errorf("held until %v, want baseRedRecheck past the verdict", until)
	}
	if !secondRes.HeldUntil.Equal(firstRes.HeldUntil) {
		t.Errorf("second hold until %v, want the recorded verdict's expiry %v", secondRes.HeldUntil, firstRes.HeldUntil)
	}
	if got := gateRuns(first, baseGateLabel); got != 1 {
		t.Errorf("first run gated the base %d times, want once", got)
	}
	if got := gateRuns(first, baseConfirmLabel); got != 1 {
		t.Errorf("first run re-ran the red base gate %d times, want once to confirm it (#189)", got)
	}
	if got := gateRuns(second, baseGateLabel) + gateRuns(second, baseConfirmLabel); got != 0 {
		t.Errorf("second run gated the same base %d times, want 0 — the verdict is keyed by base SHA and shared", got)
	}
	if len(first.Filed) != 1 || len(second.Filed) != 0 {
		t.Errorf("filed = %v / %v, want one finding for the red base, from the run that gated it", first.Filed, second.Filed)
	}
}

// A green base is gated once, and every later ticket cut from it goes straight to
// its session.
func TestImplementationGatesAGreenBaseOnce(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)

	for i, key := range []string{"PROJ-10", "PROJ-11"} {
		h := hostio.NewFake(repo.Checkout(), ticket.Slug(key))
		h.AgentFn = commitsHandoff(h, repo)
		res := Implementation(h, cfg, stageLog(t, key), Args{Identifier: key})
		if !res.OK {
			t.Fatalf("%s: a green base must dispatch, got %+v", key, res)
		}
		want := 1
		if i > 0 {
			want = 0
		}
		if got := gateRuns(h, baseGateLabel); got != want {
			t.Errorf("%s: gated the base %d times, want %d", key, got, want)
		}
		if len(h.Agents) != 1 {
			t.Errorf("%s: agent runs = %v, want the /tdd session", key, labels(h.Agents))
		}
	}
}

// The hold lasts only as long as the base it was recorded for: once main moves
// (a repair merges), the next ticket gates the new base and dispatches.
func TestImplementationDispatchesOnceMainMovesPastARedBase(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)

	held := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-20"))
	redOnBase(held)
	if res := Implementation(held, cfg, stageLog(t, "PROJ-20"), Args{Identifier: "PROJ-20"}); res.Disposition != BaseRed {
		t.Fatalf("want the ticket held on the red base, got %+v", res)
	}

	repo.Merge("main_test.go", "package main\n", "Repair the test file main no longer compiles")

	h := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-20"))
	h.AgentFn = commitsHandoff(h, repo)
	res := Implementation(h, cfg, stageLog(t, "PROJ-20"), Args{Identifier: "PROJ-20"})
	if !res.OK {
		t.Fatalf("a repaired main must dispatch the held ticket, got %+v", res)
	}
	if got := gateRuns(h, baseGateLabel); got != 1 {
		t.Errorf("gated the new base %d times, want once", got)
	}
}

// The ways past the hold: --force, the repair ticket the hold filed (it is the
// work that turns main green, so holding it would hold main red), and a resumed
// branch that already carries commits (its base is not untouched).
func TestImplementationSkipsTheBaseCheck(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *hostio.Fake, repo *gittest.Repo, args *Args)
	}{
		{"force", func(_ *hostio.Fake, _ *gittest.Repo, args *Args) { args.Force = true }},
		{"repair ticket", func(h *hostio.Fake, _ *gittest.Repo, _ *Args) {
			h.Trk.Ticket.Description = "main fails its gates.\n\n" + findings.KeyMarker(baseRedFindingKey)
		}},
		{"resumed branch", func(h *hostio.Fake, repo *gittest.Repo, _ *Args) {
			repo.Handoff(ticket.Slug("PROJ-30"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := fresh(t, "PROJ-30")
			redOnBase(h)
			h.AgentFn = commitsHandoff(h, repo)
			args := Args{Identifier: "PROJ-30"}
			tc.setup(h, repo, &args)

			res := Implementation(h, sharedCfg(t), stageLog(t, "PROJ-30"), args)

			if res.Disposition == BaseRed || len(h.Agents) != 1 {
				t.Errorf("want the session launched without a base check, got %+v, agents %v", res, labels(h.Agents))
			}
			if got := gateRuns(h, baseGateLabel); got != 0 {
				t.Errorf("gated the base %d times, want 0", got)
			}
		})
	}
}

// A red verdict can be a flake, so it is trusted only for a while; a green one
// holds for the life of its SHA.
func TestBaseVerdictsExpireRedButNotGreen(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	v := baseVerdicts{dir: t.TempDir(), now: func() time.Time { return now }}

	if _, ok := v.read("aaa"); ok {
		t.Fatal("an unrecorded base has no verdict")
	}
	v.write("aaa", baseVerdict{Gate: "check"})
	v.write("bbb", baseVerdict{})

	if got, ok := v.read("aaa"); !ok || got.Green() || got.Gate != "check" {
		t.Errorf("read(aaa) = %+v, %v; want the red verdict naming its gate", got, ok)
	}
	now = now.Add(baseRedRecheck + time.Minute)
	if _, ok := v.read("aaa"); ok {
		t.Error("a red verdict past baseRedRecheck must be gated again")
	}
	if got, ok := v.read("bbb"); !ok || !got.Green() {
		t.Errorf("read(bbb) = %+v, %v; a green verdict does not expire", got, ok)
	}
}

// #176: a red base gate's narration carries its step log's tail and path, not just
// the stderr tail, which for a guard that reports on stdout is only make's summary.
func TestImplementationRedBaseNarratesTheStepLogTailAndPath(t *testing.T) {
	repo := gittest.New(t)
	h := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-30"))
	log := stageLog(t, "PROJ-30")
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, baseGateStep) {
			_ = os.WriteFile(filepath.Join(log.Dir, run.Label+".log"), []byte("check-exec: raw exec.Command(\"gh\") in internal/ci/watch.go:88\n"), 0o644)
			return session.Outcome{ExitCode: 2, StderrTail: "make: *** [Makefile:30: check-exec] Error 1"}
		}
		return session.Outcome{}
	}

	if res := Implementation(h, sharedCfg(t), log, Args{Identifier: "PROJ-30"}); res.Disposition != BaseRed {
		t.Fatalf("want the ticket held on the red base, got %+v", res)
	}

	said := narration(t, log)
	for _, want := range []string{
		`raw exec.Command(\"gh\") in internal/ci/watch.go:88`,
		"make: *** [Makefile:30: check-exec] Error 1",
		filepath.Join(log.Dir, baseConfirmLabel+".log"),
	} {
		if !strings.Contains(said, want) {
			t.Errorf("run.jsonl should carry %q; got %s", want, said)
		}
	}
	if len(h.Filed) != 1 {
		t.Fatalf("filed = %v, want the red-base finding", h.Filed)
	}
}

// #189, replayed: a flaky gate step went red once on an untouched, green base.
// Before, that one red run was the base's verdict: the ticket was held, a
// base-gates-red finding was filed, and every ticket on the base waited out
// baseRedRecheck until the same SHA was gated again and passed. Now the red is
// re-run once before it counts; the re-run passes, so the base is recorded green,
// the ticket dispatches, nothing is filed, and the next ticket trusts the green.
func TestImplementationDispatchesWhenARedBaseGateFlakes(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)

	h := hostio.NewFake(repo.Checkout(), ticket.Slug("#187"))
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		if run.Label == baseGateLabel {
			return session.Outcome{ExitCode: 2, StderrTail: "--- FAIL: TestSlotWaitNarration (0.21s)"}
		}
		return session.Outcome{}
	}
	h.AgentFn = commitsHandoff(h, repo)
	log := stageLog(t, "#187")
	res := Implementation(h, cfg, log, Args{Identifier: "#187"})

	if !res.OK || res.Disposition == BaseRed {
		t.Fatalf("a base gate that passes its re-run must dispatch, got %+v", res)
	}
	if len(h.Agents) != 1 {
		t.Errorf("agent runs = %v, want the /tdd session", labels(h.Agents))
	}
	if got := gateRuns(h, baseConfirmLabel); got != 1 {
		t.Errorf("re-ran the red base gate %d times, want once", got)
	}
	// The session files its own dropbox after it runs; a flake must add nothing to it.
	dropbox, _ := os.ReadFile(filepath.Join(runlog.FindingsDir(log.Dir, implementationSession), "out.json"))
	if strings.Contains(string(dropbox), baseRedFindingKey) {
		t.Errorf("findings dropbox = %s, want no %s finding for a flake", dropbox, baseRedFindingKey)
	}
	if said := narration(t, log); !strings.Contains(said, "flaked") {
		t.Errorf("run.jsonl should say the base gate flaked; got %s", said)
	}

	next := hostio.NewFake(repo.Checkout(), ticket.Slug("#188"))
	redOnBase(next)
	next.AgentFn = commitsHandoff(next, repo)
	if res := Implementation(next, cfg, stageLog(t, "#188"), Args{Identifier: "#188"}); !res.OK {
		t.Fatalf("the next ticket must trust the base's green verdict, got %+v", res)
	}
	if got := gateRuns(next, baseGateLabel) + gateRuns(next, baseConfirmLabel); got != 0 {
		t.Errorf("next ticket gated the base %d times, want 0 — the flake's re-run recorded it green", got)
	}
}

// A re-run that fails environmentally confirms nothing: the base records no
// verdict, the ticket dispatches, and the next ticket gates the base afresh.
func TestImplementationDispatchesWhenARedBaseGateReRunFailsEnvironmentally(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)

	h := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-40"))
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		switch run.Label {
		case baseGateLabel:
			return session.Outcome{ExitCode: 1}
		case baseConfirmLabel:
			return session.Outcome{ExitCode: 137}
		}
		return session.Outcome{}
	}
	h.AgentFn = commitsHandoff(h, repo)
	if res := Implementation(h, cfg, stageLog(t, "PROJ-40"), Args{Identifier: "PROJ-40"}); !res.OK || res.Disposition == BaseRed {
		t.Fatalf("an unconfirmed red must dispatch, got %+v", res)
	}

	if recorded, _ := os.ReadDir(config.BaseVerdictsDir(cfg.ProjectPath)); len(recorded) != 0 {
		t.Errorf("recorded %d base verdicts, want none for an unconfirmed red", len(recorded))
	}
}
