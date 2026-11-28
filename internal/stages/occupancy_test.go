package stages

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// #481: the ticket pipeline occupies its worktree as the PR loop does (ADR-0076).
// A sweep that never reads the run lock (a Consumer's prune script, run outside
// the shield) runs during the retrospective: the worktree implementation made
// outlives it until the retrospective exits, and the retrospective's own reap
// still removes it after.
func TestTicketRunOccupiesTheWorktreeUntilTheRetrospectiveExits(t *testing.T) {
	h, repo := fresh(t, "PROJ-481")
	h.AgentFn = commitsHandoff(h, repo)
	if res := Implementation(h, stageCfg(), stageLog(t, "PROJ-481"), Args{Identifier: "PROJ-481"}); !res.OK {
		t.Fatalf("implementation: %+v", res)
	}
	if err := h.PushForceWithLease(); err != nil {
		t.Fatalf("push: %v", err)
	}
	path := h.Worktree().Path()
	sweep := func() error {
		return exec.Command("git", "-C", repo.Dir, "worktree", "remove", "--force", path).Run()
	}
	var swept, kept bool
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		kept = true
		for j := 0; j < 5; j++ {
			swept = swept || sweep() == nil
			time.Sleep(10 * time.Millisecond)
			kept = kept && h.Worktree().Exists() && h.Worktree().Clean()
		}
		return writesDropbox("[]")(i, run)
	}
	log := stageLog(t, "PROJ-481")

	if res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-481"}); !res.OK {
		t.Fatalf("retrospective: %+v", res)
	}
	if swept || !kept {
		t.Error("a sweep removed the worktree while the retrospective was still running in it")
	}
	if h.Worktree().Exists() {
		t.Errorf("the retrospective's reap removes the shipped worktree once its session has exited; run.jsonl = %s", narration(t, log))
	}
}

// #481: the occupancy lock a killed run left on a ticket's worktree is lifted
// when the next run provisions it, so this run's own lock can be taken.
func TestImplementationLiftsTheOccupancyLockAKilledRunLeftOnItsWorktree(t *testing.T) {
	h, repo := fresh(t, "PROJ-481")
	wt := h.Worktree()
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	repo.Occupy(wt)
	var occupied bool
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		occupied = repo.Occupied(wt)
		return commitsHandoff(h, repo)(i, run)
	}
	log := stageLog(t, "PROJ-481")

	if res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-481"}); !res.OK {
		t.Fatalf("implementation: %+v", res)
	}
	got := narration(t, log)
	if !strings.Contains(got, "lifted the occupancy lock a killed run left") {
		t.Errorf("the lifted lock must be narrated; run.jsonl = %s", got)
	}
	if strings.Contains(got, "could not lock the worktree") || !occupied {
		t.Errorf("the run must hold its own lock while the session runs; run.jsonl = %s", got)
	}
}

// #481: the run occupies the worktree it provisions, so the discard of an
// untouched worktree on a red base lifts that lock first, or git refuses it.
func TestImplementationDiscardsItsOccupiedWorktreeOnARedBase(t *testing.T) {
	h, repo := fresh(t, "PROJ-481")
	wt := h.Worktree()
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	repo.Occupy(wt)
	redOnBase(h)
	log := stageLog(t, "PROJ-481")

	if res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-481"}); res.Disposition != BaseRed {
		t.Fatalf("a red base holds the ticket, got %+v", res)
	}
	if wt.Exists() || wt.BranchExists() {
		t.Errorf("the untouched worktree and branch must go on a red base; run.jsonl = %s", narration(t, log))
	}
}

// #481: a run killed while its sessions were in the worktree left it locked. The
// retrospective that next reaps it, under the run lock, lifts that lock first.
func TestRetrospectiveReapsAWorktreeAKilledRunLeftOccupied(t *testing.T) {
	h := shipped(t, "PROJ-481")
	h.AgentFn = writesDropbox("[]")
	occupyAsAKilledRun(t, h)
	log := stageLog(t, "PROJ-481")

	if res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-481"}); !res.OK {
		t.Fatalf("retrospective: %+v", res)
	}
	if h.Worktree().Exists() {
		t.Errorf("the reap must lift the occupancy lock before it removes the worktree; run.jsonl = %s", narration(t, log))
	}
}

// #481: a worktree the retrospective re-creates (#384) is occupied while its
// session runs in it.
func TestRetrospectiveOccupiesTheWorktreeItRecreates(t *testing.T) {
	h, repo := handedOff(t, "PROJ-481")
	wt := h.Worktree()
	if err := h.PushForceWithLease(); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := wt.Remove(); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	var occupied bool
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		occupied = repo.Occupied(wt)
		return writesDropbox("[]")(i, run)
	}

	if res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-481"), Args{Identifier: "PROJ-481"}); !res.OK {
		t.Fatalf("retrospective: %+v", res)
	}
	if !occupied {
		t.Error("the re-created worktree must be occupied while the retrospective runs in it")
	}
	if wt.Exists() {
		t.Error("the reap still removes the re-created, shipped worktree")
	}
}

// occupyAsAKilledRun locks h's worktree as a killed run leaves it.
func occupyAsAKilledRun(t *testing.T, h *hostio.Fake) {
	t.Helper()
	wt := h.Worktree()
	if err := exec.Command("git", "-C", wt.Path(), "worktree", "lock", "--reason", gitpkg.OccupiedReason, wt.Path()).Run(); err != nil {
		t.Fatalf("lock: %v", err)
	}
}
