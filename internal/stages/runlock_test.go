package stages

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostlock"
)

type events []string

func (e *events) Event(msg string) { *e = append(*e, msg) }

// #366: a sibling run's half-finished edits were committed as this session's
// recovery checkpoint, under the wrong attribution, because the two runs shared
// the worktree. The checkpoint now skips a worktree whose run lock another run
// holds and says why; the run that holds the lock itself, or a worktree no run
// holds, still checkpoints.
func TestCheckpointSkipsAWorktreeAnotherRunHolds(t *testing.T) {
	var cfg config.Config
	cfg.ProjectPath = t.TempDir()
	lock := hostlock.File(config.RunLockFile(cfg.ProjectPath, "gh-366"))

	var log events
	if checkpointHeldElsewhere(cfg, "#366", "run-b", "/wt/gh-366", &log) {
		t.Errorf("a worktree no run holds must still be checkpointed; events = %v", log)
	}

	release, ok, err := lock.TryLockAs("run-a")
	if err != nil || !ok {
		t.Fatalf("TryLockAs() = ok %v err %v", ok, err)
	}
	defer release()
	if checkpointHeldElsewhere(cfg, "#366", "run-a", "/wt/gh-366", &log) {
		t.Errorf("the run holding the lock must checkpoint its own work; events = %v", log)
	}
	if len(log) != 0 {
		t.Errorf("events = %v, want none while the checkpoint goes ahead", log)
	}

	if !checkpointHeldElsewhere(cfg, "#366", "run-b", "/wt/gh-366", &log) {
		t.Fatal("run-b checkpointed a worktree run-a's live run holds")
	}
	if len(log) != 1 || !strings.Contains(log[0], "run-a") || !strings.Contains(log[0], "#366") || !strings.Contains(log[0], "/wt/gh-366") {
		t.Errorf("events = %v, want one line naming the holder, the ticket and the worktree", log)
	}
}
