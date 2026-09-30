package pipeline

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
)

func planCfg() config.Config {
	return config.Config{
		Host: config.Host{
			ProjectPath:         "/herd",
			ImplementationModel: "claude-opus-5-5",
			ReviewModel:         "claude-sonnet-5",
			RetrospectiveModel:  "claude-haiku-4-5-20251001",
		},
		Project: config.Project{
			Image:          "herd-agent-harness:latest",
			CacheVolume:    "herd-pnpm-store",
			CacheMountPath: "/pnpm-store",
		},
	}
}

func TestPlanNamesAllThreeStagesInOrder(t *testing.T) {
	plan := Plan(planCfg(), "beh-527")
	iImpl := strings.Index(plan, "implementation")
	iReview := strings.Index(plan, "review")
	iRetro := strings.Index(plan, "retrospective")
	if iImpl < 0 || iReview < 0 || iRetro < 0 {
		t.Fatalf("plan is missing a stage name:\n%s", plan)
	}
	if !(iImpl < iReview && iReview < iRetro) {
		t.Errorf("stages out of order in plan (impl=%d review=%d retro=%d)", iImpl, iReview, iRetro)
	}
}

// BEH-636: the harness now creates the worktree + branch host-side and runs the
// Consumer's post_create hook before the implementation session (retiring the
// sandbox-runs-new-worktree.sh coupling). The dry-run plan must be honest about
// that: it shows the host-side creation and the resolved post_create docker command.
func TestPlanShowsHostSideWorktreeProvisioning(t *testing.T) {
	cfg := planCfg()
	cfg.BranchPrefix = "feat"
	cfg.PostCreate = "cd web && pnpm install --frozen-lockfile"
	plan := Plan(cfg, "BEH-636")

	low := strings.ToLower(plan)
	if !strings.Contains(low, "host-side") || !strings.Contains(low, "worktree") {
		t.Errorf("plan does not show host-side worktree creation:\n%s", plan)
	}
	if !strings.Contains(plan, "feat/beh-636") {
		t.Errorf("plan does not name the canonical host-side branch:\n%s", plan)
	}
	if !strings.Contains(plan, "cd web && pnpm install --frozen-lockfile") {
		t.Errorf("plan does not show the resolved post_create hook command:\n%s", plan)
	}
}

func TestPlanReflectsSkipAndAlwaysSemantics(t *testing.T) {
	plan := Plan(planCfg(), "BEH-1")
	// Review is conditional on implementation succeeding; retrospective is not.
	if !strings.Contains(plan, "only if implementation") {
		t.Errorf("plan does not state review runs only if implementation succeeded:\n%s", plan)
	}
	if !strings.Contains(plan, "always") {
		t.Errorf("plan does not state retrospective always runs:\n%s", plan)
	}
}

func TestPlanNotesWorktreeAssumptionCaveat(t *testing.T) {
	plan := Plan(planCfg(), "BEH-1")
	low := strings.ToLower(plan)
	if !strings.Contains(low, "worktree") || !strings.Contains(low, "dry-run") {
		t.Errorf("plan does not note the review/retro worktree/transcript assumption under dry-run:\n%s", plan)
	}
}

func TestPlanUppercasesIdentifier(t *testing.T) {
	if plan := Plan(planCfg(), "beh-9"); !strings.Contains(plan, "BEH-9") {
		t.Errorf("plan does not surface the uppercased identifier BEH-9:\n%s", plan)
	}
}

// Plan is the three Stages' own plans concatenated, in order: every run a Stage
// plans appears as the argv the Runner previews for it, and nothing else does.
// (That a Stage's plan is what it launches is stages.TestPlanIsWhatTheStagesLaunch.)
func TestPlanRendersEachStagesOwnRuns(t *testing.T) {
	cfg := planCfg()
	cfg.PostCreate = "dotnet restore"
	cfg.Gates = []config.Gate{{Name: "test", Command: "dotnet test"}, {Name: "lint", Command: "dotnet format --verify-no-changes"}}
	plan := Plan(cfg, "proj-1")

	preview := hostio.NewPreview(cfg)
	s := stages.NewSubject(preview, "PROJ-1", runlog.TicketDir(stages.LogsRoot(cfg), "PROJ-1"), ticket.Ticket{Identifier: "PROJ-1"})
	var sessions, containers []string
	for _, p := range []stages.StagePlan{
		stages.ImplementationPlan(cfg, s, prompt.Fresh),
		stages.ReviewPlan(cfg, s),
		stages.RetrospectivePlan(cfg, s, nil),
	} {
		for _, a := range p.Agents() {
			sessions = append(sessions, "docker "+strings.Join(preview.AgentPreview(a), " "))
		}
		for _, sh := range p.Shells() {
			containers = append(containers, "docker "+strings.Join(preview.ShellPreview(sh), " "))
		}
	}
	if len(sessions) != 3 || len(containers) != 4 {
		t.Fatalf("planned %d sessions / %d containers, want 3 / 4 (postcreate, prep, 2 gates)", len(sessions), len(containers))
	}
	last := -1
	for _, w := range sessions {
		i := strings.Index(plan, w)
		if i < 0 {
			t.Fatalf("plan is missing a planned session:\n%s\n--- plan ---\n%s", w, plan)
		}
		if i < last {
			t.Errorf("sessions out of stage order in plan")
		}
		last = i
	}
	for _, w := range containers {
		if !strings.Contains(plan, w) {
			t.Errorf("plan is missing a planned container:\n%s", w)
		}
	}
	want := append(sessions, containers...)
	if got := strings.Count(plan, "docker run"); got != len(want) {
		t.Errorf("plan shows %d docker commands, the Stages plan %d", got, len(want))
	}
}

// ADR-0013: the Runner mints every container name and a caller never does. The
// plan has no run yet, so the Runner stamps placeholders where the run id and pid go.
func TestPlanContainerNamesAreMintedByTheRunner(t *testing.T) {
	plan := Plan(planCfg(), "BEH-1")
	prefix := sandbox.ContainerPrefix("/herd")
	for _, label := range []string{"implementation", "review", "retrospective"} {
		if want := "--name " + prefix + "<run-id>-<pid>-" + label + " "; !strings.Contains(plan, want) {
			t.Errorf("plan does not name the %s container %q:\n%s", label, want, plan)
		}
	}
}
