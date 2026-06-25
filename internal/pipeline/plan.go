package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/beherd/agent-harness/internal/config"
	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/prompt"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/ticket"
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
	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)
	logsRoot := filepath.Join(cfg.HerdPath, "agent-harness", "logs")

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
	implPrompt := prompt.BuildTdd(t, slug)
	implDocker := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     findingsDir("implementation"),
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          implPrompt,
		Model:           cfg.Model,
		ContainerName:   name("implementation"),
	})
	fmt.Fprintf(&b, "\n=== stage 1: implementation ===\n--- prompt ---\n%s\n\n--- docker command ---\n%s\n", implPrompt, dockerLine(implDocker))

	gateConfig := sandbox.GateConfig{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		WorktreePath:    worktreePath,
		PnpmStoreVolume: cfg.PnpmStoreVolume,
	}
	withName := func(session string) sandbox.GateConfig {
		c := gateConfig
		c.ContainerName = name(session)
		return c
	}

	// --- review ---
	reviewPrompt := prompt.BuildReview(t, slug, worktreePath)
	reviewDocker := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     "",
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          reviewPrompt,
		Model:           cfg.Model,
		ContainerName:   name("review"),
	})
	installDocker := sandbox.BuildInstallRunArgs(withName("install"))
	gateDocker := sandbox.BuildGateRunArgs(withName("gate"))
	fmt.Fprintf(&b,
		"\n=== stage 2: review (only if implementation succeeded) ===\n--- prompt ---\n%s\n\n--- install docker command ---\n%s\n\n--- review docker command ---\n%s\n\n--- gate docker command ---\n%s\n",
		reviewPrompt, dockerLine(installDocker), dockerLine(reviewDocker), dockerLine(gateDocker),
	)

	// --- retrospective ---
	// Dry-run never fetches Linear, so there's no already-filed context to inject.
	retroPrompt := prompt.BuildRetrospective(t, slug, nil)
	retroDocker := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     findingsDir("retrospective"),
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          retroPrompt,
		Model:           cfg.Model,
		ContainerName:   name("retrospective"),
	})
	fmt.Fprintf(&b, "\n=== stage 3: retrospective (always) ===\n--- prompt ---\n%s\n\n--- docker command ---\n%s\n", retroPrompt, dockerLine(retroDocker))

	fmt.Fprintf(&b,
		"\nNote: the review and retrospective commands above assume implementation's worktree (%s) and its transcripts already exist. Under dry-run they do not — implementation never ran — so those commands are the plan, not a runnable state. Prompts show no ticket title/body because Linear is not fetched under dry-run.\n",
		worktreePath,
	)

	return b.String()
}
