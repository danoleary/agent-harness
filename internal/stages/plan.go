package stages

import (
	"fmt"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// Subject is what a Stage plans its runs against: the ticket, where its worktree
// and branch live, and its log dir (where the findings dropboxes sit).
type Subject struct {
	Ticket       ticket.Ticket
	Slug         string
	WorktreePath string
	Branch       string
	LogDir       string
}

// NewSubject resolves identifier's worktree and branch through p — the run's Host,
// or a dry-run's hostio.NewPreview — so a plan never re-derives either.
func NewSubject(p hostio.Planner, identifier, logDir string, t ticket.Ticket) Subject {
	slug := ticket.Slug(identifier)
	return Subject{Ticket: t, Slug: slug, WorktreePath: p.WorktreePath(slug), Branch: p.BranchName(slug), LogDir: logDir}
}

// Step is one entry in a Stage's plan: a container the Stage launches (exactly one
// of Agent or Shell), or a Note for a host-side step with no container — or for a
// container the Stage deliberately skips, so the plan says so rather than omit it.
type Step struct {
	Title string
	Agent *hostio.AgentRun
	Shell *hostio.ShellRun
	Note  string
}

// StagePlan is a Stage's runs, in launch order, described once as the specs it
// hands the Runner. The Stage launches them; a dry-run renders them through the
// Runner's previews. Retry schedules are attached at launch, not planned: they are
// how a run is retried, not what it runs, and a retry launches the same spec.
type StagePlan struct {
	Stage string
	Steps []Step
}

// Agents is the plan's agent sessions, in order.
func (p StagePlan) Agents() []hostio.AgentRun {
	var out []hostio.AgentRun
	for _, s := range p.Steps {
		if s.Agent != nil {
			out = append(out, *s.Agent)
		}
	}
	return out
}

// Shells is the plan's worktree shell containers, in order.
func (p StagePlan) Shells() []hostio.ShellRun {
	var out []hostio.ShellRun
	for _, s := range p.Steps {
		if s.Shell != nil {
			out = append(out, *s.Shell)
		}
	}
	return out
}

// Render prints the plan for --dry-run: each agent's resolved prompt and every
// container's argv, previewed through pr so the Runner — never the Stage — names
// the container.
func (p StagePlan) Render(pr hostio.Planner) string {
	var b strings.Builder
	for _, s := range p.Steps {
		switch {
		case s.Agent != nil:
			fmt.Fprintf(&b, "\n--- %s prompt ---\n%s\n", s.Title, s.Agent.Prompt)
			fmt.Fprintf(&b, "\n--- %s docker command ---\ndocker %s\n", s.Title, strings.Join(pr.AgentPreview(*s.Agent), " "))
		case s.Shell != nil:
			fmt.Fprintf(&b, "\n--- %s docker command ---\ndocker %s\n", s.Title, strings.Join(pr.ShellPreview(*s.Shell), " "))
		default:
			fmt.Fprintf(&b, "\n--- %s ---\n%s\n", s.Title, s.Note)
		}
	}
	return b.String()
}

// ImplementationPlan is what [Implementation] launches for s: the host-side
// worktree creation, the Consumer's post_create hook, then the /tdd session.
// resume picks the Implement prompt variant (the host chooses it from the branch).
func ImplementationPlan(cfg config.Config, s Subject, resume prompt.Resume) StagePlan {
	steps := []Step{{
		Title: "worktree creation",
		Note:  fmt.Sprintf("host-side: git worktree add -b %s %s (based on origin/main), unless it already exists", s.Branch, s.WorktreePath),
	}}
	if cfg.PostCreate != "" {
		run := postCreateRun(cfg, s.WorktreePath)
		steps = append(steps, Step{Title: "post_create hook", Shell: &run})
	} else {
		steps = append(steps, Step{Title: "post_create hook", Note: "(none — no post_create hook configured)"})
	}
	run := implementationRun(cfg, s, resume)
	steps = append(steps, Step{Title: implementationSession, Agent: &run})
	return StagePlan{Stage: implementationSession, Steps: steps}
}

// ReviewPlan is what [Review] launches for s: the post_create re-provision, the
// cold review session, then one container per declared Gate, in order (BEH-634).
func ReviewPlan(cfg config.Config, s Subject) StagePlan {
	var steps []Step
	if cfg.PostCreate != "" {
		run := prepRun(cfg, s.WorktreePath)
		steps = append(steps, Step{Title: prepStep, Shell: &run})
	} else {
		steps = append(steps, Step{Title: prepStep, Note: "(none — the Consumer declares no post_create)"})
	}
	run := reviewRun(cfg, s)
	steps = append(steps, Step{Title: reviewSession, Agent: &run})
	for _, g := range cfg.Gates {
		run := gateRun(cfg, s.WorktreePath, gateStep, g)
		steps = append(steps, Step{Title: fmt.Sprintf("gate %q", g.Name), Shell: &run})
	}
	return StagePlan{Stage: reviewSession, Steps: steps}
}

// RetrospectivePlan is what [Retrospective] launches for s: one session.
func RetrospectivePlan(cfg config.Config, s Subject, filed []prompt.FiledFinding) StagePlan {
	run := retrospectiveRun(cfg, s, filed)
	return StagePlan{Stage: retrospectiveSession, Steps: []Step{{Title: retrospectiveSession, Agent: &run}}}
}

// The run builders below are the one description of each container. The plans
// above compose them and the Stage bodies launch them, so a dry-run cannot show a
// run the Stage would not launch.

// postCreateRun runs the Consumer's post_create hook in the fresh worktree.
func postCreateRun(cfg config.Config, worktreePath string) hostio.ShellRun {
	return hostio.ShellRun{Label: postCreateStep, Command: cfg.PostCreate, WorktreePath: worktreePath, Cap: cfg.TddTimeout}
}

// implementationRun is the /tdd session. A multi-file extract-and-rewire
// refactor gets a larger active-time cap (tddCap, BEH-441/BEH-688). A retry after
// a usage-policy refusal re-enters the worktree the first attempt left, so only
// that variant is told where it is (BEH-389).
func implementationRun(cfg config.Config, s Subject, resume prompt.Resume) hostio.AgentRun {
	pctx := prompt.Context{Ticket: s.Ticket, Slug: s.Slug, BranchPrefix: cfg.BranchPrefix, Body: cfg.Prompts.Implement, Resume: resume}
	if resume == prompt.AfterRefusal {
		pctx.WorktreePath = s.WorktreePath
	}
	return hostio.AgentRun{
		Label:       implementationSession,
		Model:       cfg.ImplementationModel,
		Prompt:      prompt.For(prompt.Implement, pctx),
		FindingsDir: runlog.FindingsDir(s.LogDir, implementationSession),
		Cap:         tddCap(cfg, s.Ticket),
	}
}

// prepRun re-provisions the worktree (post_create) before the cold review (BEH-490).
func prepRun(cfg config.Config, worktreePath string) hostio.ShellRun {
	return hostio.ShellRun{Label: prepStep, Command: cfg.PostCreate, WorktreePath: worktreePath, Cap: cfg.ReviewTimeout}
}

// reviewRun is the cold /review-worktree session. Review emits no findings
// (retrospective owns them), so it mounts no dropbox.
func reviewRun(cfg config.Config, s Subject) hostio.AgentRun {
	return hostio.AgentRun{
		Label: reviewSession,
		Model: cfg.ReviewModel,
		Prompt: prompt.For(prompt.Review, prompt.Context{
			Ticket: s.Ticket, Slug: s.Slug, BranchPrefix: cfg.BranchPrefix, WorktreePath: s.WorktreePath, Body: cfg.Prompts.Review,
		}),
		Cap: cfg.ReviewTimeout,
	}
}

// gateRun is the container one Gate runs in. labelPrefix distinguishes a re-gate
// (after a review re-launch or a resolved conflict) from the first gate run.
func gateRun(cfg config.Config, worktreePath, labelPrefix string, g config.Gate) hostio.ShellRun {
	return hostio.ShellRun{Label: labelPrefix + "-" + g.Name, Command: g.Command, WorktreePath: worktreePath, Cap: cfg.ReviewTimeout}
}

// retrospectiveRun is the retrospective session, handed the finding classes a
// prior run already filed as settled context (BEH-539).
func retrospectiveRun(cfg config.Config, s Subject, filed []prompt.FiledFinding) hostio.AgentRun {
	return hostio.AgentRun{
		Label: retrospectiveSession,
		Model: cfg.RetrospectiveModel,
		Prompt: prompt.For(prompt.Retrospective, prompt.Context{
			Ticket: s.Ticket, Slug: s.Slug, BranchPrefix: cfg.BranchPrefix, Body: cfg.Prompts.Retro, Filed: filed,
		}),
		FindingsDir: runlog.FindingsDir(s.LogDir, retrospectiveSession),
		Cap:         cfg.RetrospectiveTimeout,
	}
}
