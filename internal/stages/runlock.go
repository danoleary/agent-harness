package stages

import (
	"fmt"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostlock"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// checkpointHeldElsewhere reports whether key's run lock is held by a live run
// other than runID, narrating that the recovery checkpoint leaves the worktree at
// path alone when it is (#366). Uncommitted work in a worktree another run holds
// is that run's, half-finished: committing it here would land it under this
// session's name. A lock that cannot be read is treated as held elsewhere — the
// work stays uncommitted in the worktree, which loses nothing.
func checkpointHeldElsewhere(cfg config.Config, key, runID, path string, log interface{ Event(string) }) bool {
	owner, held, err := hostlock.File(config.RunLockFile(cfg.ProjectPath, ticket.Slug(key))).Holder()
	switch {
	case err != nil:
		log.Event(fmt.Sprintf("⚠ could not read whether another run holds %s's run lock (%v) — not committing the uncommitted work in %s as a recovery checkpoint (#366)", key, err, path))
		return true
	case held && owner != runID:
		log.Event(fmt.Sprintf("⚠ another live run (%s) holds %s's run lock — not committing the uncommitted work in %s as this session's recovery checkpoint (#366)", ownerName(owner), key, path))
		return true
	}
	return false
}

// ownerName is a run lock's holder for narration; a holder that took the lock
// without a name is still a live run.
func ownerName(owner string) string {
	if owner == "" {
		return "unnamed"
	}
	return "run " + owner
}
