package stages

import (
	"fmt"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
)

// occupy locks the ticket's worktree for the run, as the PR loop's run does its
// own (ADR-0076, #481): a remover that never reads the run lock then leaves it
// while a session is in it. The worktree outlives any one stage, so no vacate
// func is passed between them: the run lifts the lock with [Vacate] before each
// removal and once it ends. Any lock found here is a killed run's, or this run's
// own from an earlier pass, and `git worktree lock` refuses a locked worktree, so
// it is lifted first. A lock that cannot be taken leaves the run on the run lock
// alone.
func occupy(wt gitpkg.Worktree, log interface{ Event(string) }) {
	if lifted, err := wt.Vacate(); err != nil {
		log.Event(fmt.Sprintf("… warning: could not read whether a killed run left its occupancy lock on %s: %v", wt.Path(), err))
	} else if lifted {
		log.Event(fmt.Sprintf("↺ lifted the occupancy lock a killed run left on %s (#481)", wt.Path()))
	}
	if _, err := wt.Occupy(); err != nil {
		log.Event(fmt.Sprintf("⚠ could not lock the worktree %s for the run: %v — only the run lock keeps it while the sessions run (#481)", wt.Path(), err))
	}
}

// Vacate lifts the ticket run's occupancy lock on its worktree ([occupy]), so the
// run can remove the worktree, or leave it kept and unlocked when it ends (#481).
// The ticket pipeline calls it on every return; a standalone stage command that
// provisions or re-creates the worktree calls it as it exits.
func Vacate(wt gitpkg.Worktree, log interface{ Event(string) }) {
	if _, err := wt.Vacate(); err != nil {
		log.Event(fmt.Sprintf("⚠ could not lift the run's lock on the worktree %s: %v — the next run on the ticket lifts it (#481)", wt.Path(), err))
	}
}
