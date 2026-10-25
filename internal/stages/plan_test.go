package stages

import (
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
)

// agentsWithoutRetry / shellsWithoutRetry drop the launch-time retry schedule, which holds closures (so no
// two runs compare equal) and is how a launch is retried, not what it launches.
func agentsWithoutRetry(runs []hostio.AgentRun) []hostio.AgentRun {
	out := make([]hostio.AgentRun, len(runs))
	for i, r := range runs {
		r.Retry = hostio.Retry{}
		out[i] = r
	}
	return out
}

func shellsWithoutRetry(runs []hostio.ShellRun) []hostio.ShellRun {
	out := make([]hostio.ShellRun, len(runs))
	for i, r := range runs {
		r.Retry = hostio.Retry{}
		out[i] = r
	}
	return out
}

// The dry-run plan is not a second description of the Stages' containers: it is
// the very specs the Stages hand the Runner. Run all three over a healthy host and
// the plan must name every container they launched, in order, field for field.
func TestPlanIsWhatTheStagesLaunch(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	// Every session does its job: the /tdd session commits its handoff, the review
	// emits its verdict, and each session with a findings dropbox writes it.
	h.AgentFn = func(call int, run hostio.AgentRun) session.Outcome {
		if run.Label == implementationSession {
			commitsHandoff(h, repo)(call, run)
		}
		if run.FindingsDir != "" {
			writesDropbox("[]")(call, run)
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
	base := repo.Git(repo.Dir, "rev-parse", "origin/main")
	cfg := stageCfg()
	cfg.ImplementationModel, cfg.ReviewModel, cfg.RetrospectiveModel = "impl-model", "review-model", "retro-model"
	log := stageLog(t, "PROJ-1")
	args := Args{Identifier: "PROJ-1"}

	for _, stage := range []func(hostio.Host, config.Config, *runlog.Logger, Args) Result{Implementation, Review, Retrospective} {
		if res := stage(h, cfg, log, args); !res.OK {
			t.Fatalf("stage not OK on a healthy host: %+v\n%s", res, narration(t, log))
		}
	}

	tk, err := h.Trk.FetchTicket("PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	s := NewSubject(h, "PROJ-1", log.Dir, tk)
	// Review and retrospective scope their diff to the branch's base, resolved when
	// they run (#86); implementation has no branch to resolve one from yet.
	based := s
	based.Base = base
	var agents []hostio.AgentRun
	var shells []hostio.ShellRun
	for _, p := range []StagePlan{
		ImplementationPlan(cfg, s, prompt.Fresh),
		ReviewPlan(cfg, based, nil),
		RetrospectivePlan(cfg, based, nil),
	} {
		agents = append(agents, p.Agents()...)
		shells = append(shells, p.Shells()...)
	}

	if got, want := agentsWithoutRetry(h.Agents), agentsWithoutRetry(agents); !reflect.DeepEqual(got, want) {
		t.Errorf("agent runs launched != planned\nlaunched: %+v\nplanned:  %+v", got, want)
	}
	if got, want := shellsWithoutRetry(h.Shells), shellsWithoutRetry(shells); !reflect.DeepEqual(got, want) {
		t.Errorf("shell runs launched != planned\nlaunched: %+v\nplanned:  %+v", got, want)
	}
	if len(agents) != 3 || len(shells) != 3 || len(h.Agents) != 3 {
		t.Fatalf("plan has %d agent / %d shell runs, want 3 / 3 (impl, review, retro / postcreate, prep, gate)", len(agents), len(shells))
	}
	// Plan and launch share one builder, so agreement alone can't catch a builder
	// that pins the wrong model: each session must carry its own Stage's model.
	for i, want := range []string{"impl-model", "review-model", "retro-model"} {
		if got := h.Agents[i].Model; got != want {
			t.Errorf("session %q launched with model %q, want %q", h.Agents[i].Label, got, want)
		}
	}
}

// A standalone Stage's --dry-run renders its plan through the Runner's previews —
// the Stage never spells a docker command, or a container name, itself.
func TestStageDryRunRendersItsPlanThroughTheRunner(t *testing.T) {
	cfg := stageCfg()
	h, _ := fresh(t, "PROJ-1")
	s := NewSubject(h, "PROJ-1", "/logs/PROJ-1", hostio.NewFakeTracker().Ticket)
	out := ReviewPlan(cfg, s, nil).Render(h)
	for _, want := range []string{
		"--- prep docker command ---\ndocker run --name fake-prep",
		"--- review docker command ---\ndocker run --name fake-review",
		`--- gate "check" docker command ---` + "\ndocker run --name fake-gate-check",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered review plan missing %q:\n%s", want, out)
		}
	}
}

// A Consumer with nothing to provision gets no prep container; the plan says so
// rather than preview one the run will never launch.
func TestReviewPlanNotesASkippedPrep(t *testing.T) {
	cfg := stageCfg()
	cfg.PostCreate = ""
	h, _ := fresh(t, "PROJ-1")
	p := ReviewPlan(cfg, NewSubject(h, "PROJ-1", "/logs/PROJ-1", hostio.NewFakeTracker().Ticket), nil)
	for _, sh := range p.Shells() {
		if sh.Label == prepStep {
			t.Errorf("plan launches a prep container with no post_create: %+v", sh)
		}
	}
	if out := p.Render(h); !strings.Contains(out, "no post_create") {
		t.Errorf("plan does not say the prep is skipped:\n%s", out)
	}
}
