package stages

import (
	"encoding/json"
	"os"
	"path/filepath"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/runlog"
)

// handoffFile is the per-ticket record of the last implementation session's
// verified handoff: the HEAD it left, kept in the ticket's log dir so it outlives
// the run (#367, ADR-0062). A session that exited 0 over a handoff verify.Tdd
// passed writes it; every launch of a session clears it first, so it only ever
// describes the latest session.
const handoffFile = "handoff.json"

type handoffRecord struct {
	Head string `json:"head"`
}

// recordHandoff records head as the verified handoff of the ticket whose log dir
// is logDir.
func recordHandoff(logDir, head string) error {
	raw, err := json.Marshal(handoffRecord{Head: head})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(logDir, handoffFile), raw, 0o644)
}

// clearHandoff forgets the ticket's verified handoff: a new session is about to
// launch, and whatever it does is the latest session's word. Forgetting a record
// that is not there is a no-op.
func clearHandoff(logDir string) { os.Remove(filepath.Join(logDir, handoffFile)) }

// standingHandoff returns the verified handoff a resumed branch still stands on,
// ok=false unless all of these hold, read from git rather than the record alone
// (ADR-0016):
//
//   - the worktree exists, is clean and its branch is ahead of origin/main;
//   - the last implementation session exited 0 and verify.Tdd passed its
//     handoff, which recorded the HEAD it left;
//   - the branch still carries that commit, and nothing has landed on it since
//     but harness recovery checkpoints;
//   - no review's host Gate re-run went red over it.
//
// A branch a rebase rewrote no longer carries the recorded commit, so it reads as
// not standing and the run starts at implementation, as before #367.
func standingHandoff(wt gitpkg.Worktree, logDir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(logDir, handoffFile))
	if err != nil {
		return "", false
	}
	var r handoffRecord
	if json.Unmarshal(raw, &r) != nil || r.Head == "" {
		return "", false
	}
	if !wt.Exists() || !wt.Clean() || wt.CommitsAhead() == 0 {
		return "", false
	}
	since, ok := wt.SubjectsSince(r.Head)
	if !ok {
		return "", false
	}
	for _, s := range since {
		if !gitpkg.IsCheckpoint(s) {
			return "", false
		}
	}
	// verify.Tdd passes a handoff on its commits alone. A review whose host Gate
	// re-run went red over it has shown the work is not finished, and only an
	// implementation session can fix the handoff's own red: starting at review
	// would fail on the same Gate every re-run.
	if g := LastPushGate(logDir); g != nil && g.GateRan && g.GateExit != 0 {
		if head, _ := wt.HeadSHA(); g.Head == r.Head || g.Head == head {
			return "", false
		}
	}
	return r.Head, true
}

// recordVerifiedHandoff records wt's HEAD as the verified handoff a re-run may
// start at review from. Best-effort: a failed write costs at most one more
// implementation session, as before #367.
func recordVerifiedHandoff(wt gitpkg.Worktree, log *runlog.Logger) {
	head, err := wt.HeadSHA()
	if err == nil {
		err = recordHandoff(log.Dir, head)
	}
	if err != nil {
		log.Event("⚠ could not record the verified handoff (" + err.Error() + ") — a re-run of this ticket launches an implementation session again")
	}
}
