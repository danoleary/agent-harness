package stages

import (
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
)

// #431, the incident: the implementation session committed its first slice, then
// hit its wall-clock cap (exit 137) in its final gates with the last slice green
// but uncommitted. The post-session check read "one commit ahead" and passed it,
// and review got the normal prompt over a dirty tree. Now the harness sees the
// dirty tree, records it in run.jsonl, commits it as a recovery checkpoint, and
// the review prompt names that checkpoint and its paths as unhanded-off work.
func TestACapKilledSessionsUncommittedSliceReachesReviewNamed(t *testing.T) {
	h, repo := fresh(t, "PROJ-431")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		repo.Commit(h.Worktree(), "fix.txt", "the first slice\n", gittest.HandoffSubject)
		repo.Dirty(h.Worktree())
		return session.Outcome{ExitCode: sandbox.ExitOOMKill, CapKilled: true}
	}
	log := stageLog(t, "PROJ-431")

	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-431"})

	if !res.OK {
		t.Fatalf("the committed slice is still a handoff for review, got %+v", res)
	}
	wt := h.Worktree()
	if want := strings.SplitN(gitpkg.CheckpointMessage("PROJ-431", "tdd"), "\n", 2)[0]; repo.Subject(wt) != want || !wt.Clean() {
		t.Errorf("the uncommitted slice must be checkpoint-committed before review; HEAD is %q, clean=%v", repo.Subject(wt), wt.Clean())
	}
	if got := narration(t, log); !strings.Contains(got, "uncommitted.txt") || !strings.Contains(got, "exit 137") {
		t.Errorf("run.jsonl must record the dirty tree, its paths and the exit; got %s", got)
	}
	if _, ok := standingHandoff(wt, log.Dir); ok {
		t.Error("a handoff that left work uncommitted is not a verified one a re-run may start review from")
	}

	h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{ReviewVerdictEmitted: true} }
	Review(h, stageCfg(), log, Args{Identifier: "PROJ-431"})

	review := h.Agents[len(h.Agents)-1]
	if review.Label != reviewSession {
		t.Fatalf("agent runs = %v, want the review last", labels(h.Agents))
	}
	for _, want := range []string{"UNHANDED-OFF WORK", "`uncommitted.txt`"} {
		if !strings.Contains(review.Prompt, want) {
			t.Errorf("the review prompt must name the unhanded work: missing %q", want)
		}
	}
	if strings.Contains(review.Prompt, "`fix.txt`") {
		t.Error("the handed-off slice is not unhanded work")
	}
}

// A session that committed everything hands review the normal prompt.
func TestAReviewOverACleanHandoffIsToldOfNoUnhandedWork(t *testing.T) {
	h, _ := handedOff(t, "PROJ-432")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{ReviewVerdictEmitted: true} }

	Review(h, stageCfg(), stageLog(t, "PROJ-432"), Args{Identifier: "PROJ-432"})

	if p := h.Agents[0].Prompt; strings.Contains(p, "UNHANDED-OFF WORK") {
		t.Error("a handed-off tip has no unhanded work to name")
	}
}

// A session that exits 0 but leaves work uncommitted hands off no verified
// handoff either: its tip is a checkpoint, and a re-run starts at implementation,
// not at a review that would ship work no session finished (#367, #431).
func TestAnExitZeroSessionThatLeftWorkUncommittedRecordsNoVerifiedHandoff(t *testing.T) {
	h, repo := fresh(t, "PROJ-433")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		repo.Commit(h.Worktree(), "fix.txt", "the first slice\n", gittest.HandoffSubject)
		repo.Dirty(h.Worktree())
		return session.Outcome{}
	}
	log := stageLog(t, "PROJ-433")

	if res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-433"}); !res.OK {
		t.Fatalf("the committed slice is still a handoff for review, got %+v", res)
	}
	if !strings.Contains(narration(t, log), "exited 0 with work uncommitted") {
		t.Errorf("run.jsonl must record the dirty tree; got %s", narration(t, log))
	}
	if _, ok := standingHandoff(h.Worktree(), log.Dir); ok {
		t.Error("a handoff that left work uncommitted is not a verified one a re-run may start review from")
	}
}
