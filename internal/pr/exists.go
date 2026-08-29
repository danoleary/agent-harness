package pr

import (
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/proc"
)

// ghTimeout bounds the `gh pr view` lookup so a stalled network or a blocking gh
// auth prompt can't hang the caller (same hang class the review stage bounds with
// prCreateTimeout).
const ghTimeout = 2 * time.Minute

// stateReader fetches a branch's PR state. Injected so the predicates below are
// testable without a gh binary or a network.
type stateReader func(herdPath, branch string) (string, error)

// ghState reads the PR state for a branch via `gh pr view <branch> --json state`.
// A branch with no PR makes gh exit non-zero, which surfaces here as an error.
func ghState(herdPath, branch string) (string, error) {
	out, err := proc.CombinedOutputInDir(ghTimeout, herdPath, "gh", "pr", "view", branch, "--json", "state", "-q", ".state")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Exists reports whether the branch has reached a PR at all — OPEN, MERGED, or
// CLOSED. This is the "did the work escape the worktree?" question: once a PR
// exists it captures the branch, so the worktree is pure disk cost. Distinct from
// OpenExists, which asks the narrower "is there something still in flight?".
//
// Any error (no PR for the branch, gh failure, timeout) is treated as "no PR", so
// the caller errs toward keeping the worktree — a recoverable breadcrumb is always
// cheaper than losing one.
func Exists(herdPath, branch string) bool { return exists(herdPath, branch, ghState) }

func exists(herdPath, branch string, read stateReader) bool {
	state, err := read(herdPath, branch)
	if err != nil {
		return false
	}
	switch state {
	case "OPEN", "MERGED", "CLOSED":
		return true
	default:
		return false
	}
}

// OpenExists reports whether an OPEN PR exists for the branch. Any error is
// treated as "no open PR": a caller recovering a stranded branch then proceeds to
// open one, and a duplicate `gh pr create` fails harmlessly.
func OpenExists(herdPath, branch string) bool { return openExists(herdPath, branch, ghState) }

func openExists(herdPath, branch string, read stateReader) bool {
	state, err := read(herdPath, branch)
	return err == nil && state == "OPEN"
}
