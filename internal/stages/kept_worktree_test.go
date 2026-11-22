package stages

import (
	"os"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// #384, twice in herd: review logged "keeping PR + worktree", and seconds later the
// retrospective warned the worktree was not on disk and started its session in the
// host checkout, with the checkout's stale skills, while the branch still stood at
// the reviewed head. Replayed: the worktree is removed from under the run after
// review, its branch left in place. The retrospective re-creates the worktree from
// the branch, says so, and launches its session in it; the reap then treats it as
// any shipped worktree.
func TestRetrospectiveRecreatesAKeptWorktreeRemovedFromUnderTheRun(t *testing.T) {
	h := shipped(t, "PROJ-384")
	wt := h.Worktree()
	head, _ := wt.HeadSHA()
	if err := wt.Remove(); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	var launchedIn string
	var onDisk bool
	h.AgentFn = func(i int, run hostio.AgentRun) session.Outcome {
		launchedIn = run.WorkDir
		info, err := os.Stat(run.WorkDir)
		onDisk = err == nil && info.IsDir()
		return writesDropbox("[]")(i, run)
	}
	log := stageLog(t, "PROJ-384")

	if res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-384"}); !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if launchedIn != wt.Path() || !onDisk {
		t.Fatalf("session launched in %q (on disk %v), want the re-created worktree %s", launchedIn, onDisk, wt.Path())
	}
	got := narration(t, log)
	if !strings.Contains(got, "re-created it from "+wt.Branch()) {
		t.Errorf("run.jsonl = %s, want the re-creation narrated", got)
	}
	if !strings.Contains(got, "worktree removed") {
		t.Errorf("run.jsonl = %s, want the re-created worktree reaped like any shipped one", got)
	}
	if tip, _ := h.Worktree().HeadSHA(); h.Worktree().Exists() && tip != head {
		t.Errorf("the re-created worktree is at %s, want the branch's head %s", tip, head)
	}
}

// A worktree removed from under the run during review is named at the end of the
// stage, whichever way review left: every path keeps the worktree for the
// retrospective, so one gone by then was removed by something outside the run.
func TestReviewNarratesAKeptWorktreeGoneByTheEndOfTheStage(t *testing.T) {
	h, _ := handedOff(t, "PROJ-385")
	h.ShellFn = func(_ int, s hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(s.Label, gateStep) {
			_ = h.Worktree().Remove()
		}
		return session.Outcome{}
	}
	log := stageLog(t, "PROJ-385")

	Review(h, stageCfg(), log, Args{Identifier: "PROJ-385"})

	if got := narration(t, log); !strings.Contains(got, "is gone at the end of review") {
		t.Errorf("run.jsonl = %s, want the missing kept worktree named", got)
	}
}

// The green path keeps its worktree, and says nothing about it.
func TestReviewSaysNothingOfAKeptWorktreeStillOnDisk(t *testing.T) {
	h, _ := handedOff(t, "PROJ-386")
	log := stageLog(t, "PROJ-386")

	if res := Review(h, stageCfg(), log, Args{Identifier: "PROJ-386"}); !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if got := narration(t, log); strings.Contains(got, "is gone at the end of review") {
		t.Errorf("run.jsonl = %s, want no warning over a worktree still on disk", got)
	}
}
