package stages

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
)

// noSession fails the test if a re-run launches an implementation session.
func noSession(t *testing.T) func(int, hostio.AgentRun) session.Outcome {
	return func(int, hostio.AgentRun) session.Outcome {
		t.Error("a branch that stands on its verified handoff must launch no implementation session (#367)")
		return session.Outcome{}
	}
}

// The #367 incident: an implementation session's handoff is verified; a later
// session on the same branch confirms the work and adds one more commit, which is
// verified too; the next run must then start at review rather than spend a third
// session re-running the gates over finished work. The run log names the commit
// the skip relied on.
func TestImplementationStartsAtReviewOnABranchThatStandsOnItsVerifiedHandoff(t *testing.T) {
	h, repo := fresh(t, "#367")
	log := stageLog(t, "#367")
	h.AgentFn = commitsHandoff(h, repo)
	if res := Implementation(h, stageCfg(), log, Args{Identifier: "#367"}); !res.OK {
		t.Fatalf("first run: want a verified handoff, got %+v", res)
	}
	repo.Commit(h.Worktree(), "more.txt", "a later commit\n", "a commit after the handoff")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		repo.Commit(h.Worktree(), "confirm.txt", "confirmed\n", "confirm the work is complete (#367)")
		return session.Outcome{}
	}
	if res := Implementation(h, stageCfg(), log, Args{Identifier: "#367"}); !res.OK {
		t.Fatalf("second run: want a verified handoff, got %+v", res)
	}
	handoff, _ := h.Worktree().HeadSHA()
	h.AgentFn = noSession(t)
	sessions := len(h.Agents)

	res := Implementation(h, stageCfg(), log, Args{Identifier: "#367"})

	if !res.OK {
		t.Errorf("a branch on its verified handoff starts at review: want OK, got %+v", res)
	}
	if len(h.Agents) != sessions {
		t.Errorf("agent runs = %v, want none launched by the re-run", labels(h.Agents[sessions:]))
	}
	if got := narration(t, log); !strings.Contains(got, "skipping the implementation session") || !strings.Contains(got, shortCommit(handoff)) {
		t.Errorf("the run log must name the handoff %s the skip relied on; run.jsonl = %s", shortCommit(handoff), got)
	}
}

// A harness recovery checkpoint after the verified handoff is not new work: the
// re-run still starts at review, and review sees the checkpoint.
func TestImplementationStartsAtReviewOverACheckpointAfterTheHandoff(t *testing.T) {
	h, repo := fresh(t, "#367")
	log := stageLog(t, "#367")
	h.AgentFn = commitsHandoff(h, repo)
	if res := Implementation(h, stageCfg(), log, Args{Identifier: "#367"}); !res.OK {
		t.Fatalf("first run: want a verified handoff, got %+v", res)
	}
	repo.Dirty(h.Worktree())
	if err := h.Worktree().Checkpoint("#367", "review"); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	h.AgentFn = noSession(t)

	if res := Implementation(h, stageCfg(), log, Args{Identifier: "#367"}); !res.OK || len(h.Agents) != 1 {
		t.Errorf("want OK with only the first run's session, got %+v and runs %v", res, labels(h.Agents))
	}
}

// Every way the branch does not stand on a verified handoff starts the re-run at
// implementation, as before #367.
func TestImplementationLaunchesASessionWhenTheBranchHasMovedOffItsVerifiedHandoff(t *testing.T) {
	for name, since := range map[string]func(h *hostio.Fake, repo *gittest.Repo, log *runlog.Logger){
		"a commit after the handoff": func(h *hostio.Fake, repo *gittest.Repo, _ *runlog.Logger) {
			repo.Commit(h.Worktree(), "more.txt", "more\n", "a human's follow-up")
		},
		"a later session that did not exit OK": func(h *hostio.Fake, repo *gittest.Repo, log *runlog.Logger) {
			repo.Commit(h.Worktree(), "more.txt", "more\n", "a human's follow-up")
			h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{ExitCode: 1} }
			Implementation(h, stageCfg(), log, Args{Identifier: "#367"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, repo := fresh(t, "#367")
			log := stageLog(t, "#367")
			h.AgentFn = commitsHandoff(h, repo)
			if res := Implementation(h, stageCfg(), log, Args{Identifier: "#367"}); !res.OK {
				t.Fatalf("first run: want a verified handoff, got %+v", res)
			}
			since(h, repo, log)
			h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{} }
			sessions := len(h.Agents)

			Implementation(h, stageCfg(), log, Args{Identifier: "#367"})

			if len(h.Agents) != sessions+1 {
				t.Errorf("agent runs = %v, want the re-run to launch an implementation session", labels(h.Agents[sessions:]))
			}
		})
	}
}

// A resumed branch ahead of main that no verified handoff stands under — work an
// earlier harness, a human or a crashed session left — starts at implementation.
func TestImplementationLaunchesASessionOnABranchWithNoVerifiedHandoff(t *testing.T) {
	h, _ := handedOff(t, "#367")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{} }

	Implementation(h, stageCfg(), stageLog(t, "#367"), Args{Identifier: "#367"})

	if got := labels(h.Agents); len(got) != 1 || got[0] != implementationSession {
		t.Errorf("agent runs = %v, want one implementation session", got)
	}
}
