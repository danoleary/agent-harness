package stages

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
)

// #245: the implementation and review sessions started in the operator's host
// checkout, so Claude Code loaded /tdd and /review-worktree from a side branch
// that predated the base commit's skill change, and the implementation session
// ran the full gate before committing — the very order that change removed. Each
// of a ticket's sessions now starts in the ticket's worktree, as its gates do, and
// every path its envelope names is absolute, since the session no longer starts
// where a checkout-relative path resolves.
func TestEveryTicketSessionStartsInItsWorktree(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	h.AgentFn = func(call int, run hostio.AgentRun) session.Outcome {
		if run.Label == implementationSession {
			commitsHandoff(h, repo)(call, run)
		}
		if run.FindingsDir != "" {
			writesDropbox("[]")(call, run)
		}
		return session.Outcome{ReviewVerdictEmitted: true}
	}
	cfg := stageCfg()
	log := stageLog(t, "PROJ-1")
	for _, stage := range []func(hostio.Host, config.Config, *runlog.Logger, Args) Result{Implementation, Review, Retrospective} {
		if res := stage(h, cfg, log, Args{Identifier: "PROJ-1"}); !res.OK {
			t.Fatalf("stage not OK on a healthy host: %+v\n%s", res, narration(t, log))
		}
	}

	wt := h.Worktree().Path()
	if !filepath.IsAbs(wt) {
		t.Fatalf("worktree path %q is not absolute", wt)
	}
	if len(h.Agents) != 3 {
		t.Fatalf("launched %d sessions, want 3", len(h.Agents))
	}
	for _, run := range h.Agents {
		if run.WorkDir != wt {
			t.Errorf("%s session starts in %q, want the worktree %q", run.Label, run.WorkDir, wt)
		}
		if strings.Contains(run.Prompt, "`.claude/worktrees/") {
			t.Errorf("%s prompt names the worktree relative to the checkout:\n%s", run.Label, run.Prompt)
		}
	}
	if impl := h.Agents[0].Prompt; !strings.Contains(impl, "`"+wt+"`") {
		t.Errorf("implementation prompt does not name the worktree by its absolute path %q:\n%s", wt, impl)
	}
	retro := h.Agents[2].Prompt
	if !strings.Contains(retro, "`"+log.Dir+"/`") {
		t.Errorf("retrospective prompt does not name the log dir by its absolute path %q:\n%s", log.Dir, retro)
	}
}
