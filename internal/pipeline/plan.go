package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// Plan renders the pipeline's dry-run plan for identifier: the ordered stages
// with their skip/always semantics, each stage's resolved prompt + docker
// command, and the caveat that review/retro assume impl's worktree + transcripts
// which don't exist under dry-run. It is pure — it claims nothing, launches
// nothing, and touches no Linear (the prompts are built from a stub ticket, so
// the title/description are blank), which is why it lives apart from Run.
//
// The container names use literal <run-id>/<pid> placeholders rather than minting
// a real run id: a real run resolves those at launch, and a deterministic plan is
// both honest about dry-run and trivially testable.
func Plan(cfg config.Config, identifier string) string {
	id := strings.ToUpper(identifier)
	slug := strings.ToLower(identifier)
	// Stub ticket: Linear is deliberately not fetched under dry-run, so the prompt
	// bodies show structure with an empty title/description ("best computable").
	t := ticket.Ticket{Identifier: id}
	worktreePath := gitpkg.WorktreePath(cfg.ProjectPath, slug)
	logsRoot := filepath.Join(config.ProjectDir(cfg.ProjectPath), "logs")

	name := func(session string) string {
		return fmt.Sprintf("herd-harness-<run-id>-<pid>-%s", session)
	}
	findingsDir := func(session string) string {
		return filepath.Join(logsRoot, id, "findings", session)
	}
	dockerLine := func(args []string) string {
		return "docker " + strings.Join(args, " ")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "pipeline %s — dry-run plan (nothing launched, no ticket claimed, Linear untouched)\n", id)
	b.WriteString("\nstages, in order:\n")
	b.WriteString("  1. implementation  — always runs first\n")
	b.WriteString("  2. review          — only if implementation succeeded\n")
	b.WriteString("  3. retrospective   — always runs, even after a prior-stage failure\n")
	b.WriteString("exit 0 iff every stage that ran succeeded.\n")

	// --- implementation ---
	implPrompt := prompt.BuildTdd(t, slug, cfg.BranchPrefix, cfg.Prompts.Implement)
	implDocker := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:          cfg.Image,
		ProjectPath:    cfg.ProjectPath,
		FindingsDir:    findingsDir("implementation"),
		CacheVolume:    cfg.CacheVolume,
		CacheMountPath: cfg.CacheMountPath,
		Prompt:         implPrompt,
		Model:          cfg.Model,
		ContainerName:  name("implementation"),
	})
	// The harness creates the worktree + canonical branch host-side, then runs the
	// Consumer's post_create hook, BEFORE the session (BEH-636) — show both so the
	// plan is honest about the full lifecycle, not just the model session.
	worktreeCreate := fmt.Sprintf("host-side: git worktree add -b %s %s (based on origin/main)", gitpkg.BranchName(cfg.BranchPrefix, slug), worktreePath)
	postCreateLine := "(none — no post_create hook configured)"
	if cfg.PostCreate != "" {
		postCreateLine = dockerLine(sandbox.BuildPostCreateRunArgs(sandbox.GateConfig{
			Image:          cfg.Image,
			ProjectPath:    cfg.ProjectPath,
			WorktreePath:   worktreePath,
			CacheVolume:    cfg.CacheVolume,
			CacheMountPath: cfg.CacheMountPath,
			ContainerName:  name("postcreate"),
		}, cfg.PostCreate))
	}
	fmt.Fprintf(&b, "\n=== stage 1: implementation ===\n--- worktree creation ---\n%s\n\n--- post_create hook ---\n%s\n\n--- prompt ---\n%s\n\n--- docker command ---\n%s\n", worktreeCreate, postCreateLine, implPrompt, dockerLine(implDocker))

	gateConfig := sandbox.GateConfig{
		Image:          cfg.Image,
		ProjectPath:    cfg.ProjectPath,
		WorktreePath:   worktreePath,
		CacheVolume:    cfg.CacheVolume,
		CacheMountPath: cfg.CacheMountPath,
	}
	withName := func(session string) sandbox.GateConfig {
		c := gateConfig
		c.ContainerName = name(session)
		return c
	}

	// --- review ---
	reviewPrompt := prompt.BuildReview(t, slug, worktreePath, cfg.BranchPrefix, cfg.Prompts.Review)
	reviewDocker := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:          cfg.Image,
		ProjectPath:    cfg.ProjectPath,
		FindingsDir:    "",
		CacheVolume:    cfg.CacheVolume,
		CacheMountPath: cfg.CacheMountPath,
		Prompt:         reviewPrompt,
		Model:          cfg.Model,
		ContainerName:  name("review"),
	})
	installDocker := sandbox.BuildInstallRunArgs(withName("install"))
	// One gate container per config-declared named gate, run in order (BEH-634).
	gateLines := make([]string, 0, len(cfg.Gates))
	for _, g := range cfg.Gates {
		gateDocker := sandbox.BuildGateRunArgs(withName("gate-"+g.Name), g.Command)
		gateLines = append(gateLines, fmt.Sprintf("# gate %q\n%s", g.Name, dockerLine(gateDocker)))
	}
	fmt.Fprintf(&b,
		"\n=== stage 2: review (only if implementation succeeded) ===\n--- prompt ---\n%s\n\n--- install docker command ---\n%s\n\n--- review docker command ---\n%s\n\n--- gate docker commands ---\n%s\n",
		reviewPrompt, dockerLine(installDocker), dockerLine(reviewDocker), strings.Join(gateLines, "\n\n"),
	)

	// --- retrospective ---
	// Dry-run never fetches Linear, so there's no already-filed context to inject.
	retroPrompt := prompt.BuildRetrospective(t, slug, nil, cfg.BranchPrefix, cfg.Prompts.Retro)
	retroDocker := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:          cfg.Image,
		ProjectPath:    cfg.ProjectPath,
		FindingsDir:    findingsDir("retrospective"),
		CacheVolume:    cfg.CacheVolume,
		CacheMountPath: cfg.CacheMountPath,
		Prompt:         retroPrompt,
		Model:          cfg.Model,
		ContainerName:  name("retrospective"),
	})
	fmt.Fprintf(&b, "\n=== stage 3: retrospective (always) ===\n--- prompt ---\n%s\n\n--- docker command ---\n%s\n", retroPrompt, dockerLine(retroDocker))

	fmt.Fprintf(&b,
		"\nNote: the review and retrospective commands above assume implementation's worktree (%s) and its transcripts already exist. Under dry-run they do not — implementation never ran — so those commands are the plan, not a runnable state. Prompts show no ticket title/body because Linear is not fetched under dry-run.\n",
		worktreePath,
	)

	return b.String()
}
