package pipeline

import (
	"fmt"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// Plan renders the pipeline's dry-run plan for identifier: the ordered stages
// with their skip/always semantics, then each Stage's own plan — the run specs it
// would launch — and the caveat that review/retro assume impl's worktree +
// transcripts, which don't exist under dry-run. It is pure — it claims nothing,
// launches nothing, and touches no tracker (the prompts are built from a stub
// ticket, so the title/description are blank), which is why it lives apart from Run.
//
// Plan holds no knowledge of its own about any container: the Stages describe
// their runs (stages.ImplementationPlan & co.), and hostio.NewPreview renders them
// through the Runner, which stamps <run-id>/<pid> placeholders into the names it
// mints because no run exists yet (ADR-0018).
func Plan(cfg config.Config, identifier string) string {
	id := strings.ToUpper(identifier)
	preview := hostio.NewPreview(cfg)
	// Stub ticket: the tracker is deliberately not fetched under dry-run, so the
	// prompt bodies show structure with an empty title/description.
	s := stages.NewSubject(preview, id, runlog.TicketDir(stages.LogsRoot(cfg), id), ticket.Ticket{Identifier: id})

	var b strings.Builder
	fmt.Fprintf(&b, "pipeline %s — dry-run plan (nothing launched, no ticket claimed, tracker untouched)\n", id)
	b.WriteString("\nstages, in order:\n")
	b.WriteString("  1. implementation  — always runs first\n")
	b.WriteString("  2. review          — only if implementation succeeded\n")
	b.WriteString("  3. retrospective   — always runs, even after a prior-stage failure\n")
	b.WriteString("exit 0 iff every stage that ran succeeded.\n")

	for i, st := range []struct {
		plan stages.StagePlan
		when string
	}{
		{stages.ImplementationPlan(cfg, s, prompt.Fresh), "always"},
		{stages.ReviewPlan(cfg, s), "only if implementation succeeded"},
		// Dry-run never fetches the tracker, so there's no already-filed context to inject.
		{stages.RetrospectivePlan(cfg, s, nil), "always"},
	} {
		fmt.Fprintf(&b, "\n=== stage %d: %s (%s) ===\n", i+1, st.plan.Stage, st.when)
		b.WriteString(st.plan.Render(preview))
	}

	fmt.Fprintf(&b,
		"\nNote: the review and retrospective commands above assume implementation's worktree (%s) and its transcripts already exist. Under dry-run they do not — implementation never ran — so those commands are the plan, not a runnable state. Prompts show no ticket title/body because the tracker is not fetched under dry-run.\n",
		s.WorktreePath,
	)
	return b.String()
}
