package pipeline

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
)

func planCfg() config.Config {
	return config.Config{
		Image:               "herd-agent-harness:latest",
		ProjectPath:         "/herd",
		CacheVolume:         "herd-pnpm-store",
		CacheMountPath:      "/pnpm-store",
		ImplementationModel: "claude-opus-5-5",
		ReviewModel:         "claude-sonnet-5",
		RetrospectiveModel:  "claude-haiku-4-5-20251001",
	}
}

// Each stage's docker command carries that stage's model, never another's.
func TestPlanPinsEachStageToItsOwnModel(t *testing.T) {
	plan := Plan(planCfg(), "beh-527")
	iReview := strings.Index(plan, "=== stage 2: review")
	iRetro := strings.Index(plan, "=== stage 3: retrospective")
	if iReview < 0 || iRetro < 0 {
		t.Fatalf("plan is missing a stage header:\n%s", plan)
	}
	for _, c := range []struct{ section, want string }{
		{plan[:iReview], "--model claude-opus-5-5"},
		{plan[iReview:iRetro], "--model claude-sonnet-5"},
		{plan[iRetro:], "--model claude-haiku-4-5-20251001"},
	} {
		if !strings.Contains(c.section, c.want) || strings.Count(c.section, "--model ") != 1 {
			t.Errorf("stage section should pin exactly %q:\n%s", c.want, c.section)
		}
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

func TestPlanIncludesResolvedDockerCommandsPerStage(t *testing.T) {
	plan := Plan(planCfg(), "BEH-1")
	if n := strings.Count(plan, "docker run"); n < 3 {
		t.Errorf("plan has %d `docker run` commands, want at least one per stage (>=3):\n%s", n, plan)
	}
	// The resolved command must carry the configured image — proof it is computed,
	// not a placeholder.
	if !strings.Contains(plan, "herd-agent-harness:latest") {
		t.Errorf("plan docker commands do not reference the configured image:\n%s", plan)
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

// --- the review prep container is Consumer-declared (BEH-641) ---
//
// The prep that runs before the cold review session was a hardcoded
// `cd web && pnpm install --frozen-lockfile` inside internal/sandbox — narrated
// verbatim to a .NET Consumer, in a package CONTEXT.md says must never name
// pnpm. It is the Consumer's post_create hook now, the same idempotent
// provisioning the implementation stage runs.

func TestPlanReviewPrepRunsTheConsumerPostCreate(t *testing.T) {
	cfg := planCfg()
	cfg.PostCreate = "dotnet restore"
	plan := Plan(cfg, "PROJ-1")

	if !strings.Contains(plan, "prep docker command") {
		t.Fatalf("plan must show the review prep container:\n%s", plan)
	}
	if !strings.Contains(plan, "dotnet restore") {
		t.Errorf("review prep must run the Consumer's post_create:\n%s", plan)
	}
	if strings.Contains(plan, "pnpm install") {
		t.Errorf("no compiled-in pnpm install may survive in the plan:\n%s", plan)
	}
}

// A Consumer with nothing to provision gets no prep container, and the plan must
// say so rather than print one the run will never launch.
func TestPlanReviewPrepSkippedWithNoPostCreate(t *testing.T) {
	cfg := planCfg()
	cfg.PostCreate = ""
	plan := Plan(cfg, "PROJ-1")

	if !strings.Contains(plan, "no post_create") {
		t.Errorf("plan must say the prep container is skipped when no post_create is declared:\n%s", plan)
	}
}
