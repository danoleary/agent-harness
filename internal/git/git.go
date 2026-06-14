// Package git gathers the ground truth a finished tdd session leaves behind,
// read from the host's primary checkout.
package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/beherd/agent-harness/internal/verify"
)

// WorktreePath is the host path of the worktree the tdd skill is told to create.
func WorktreePath(herdPath, slug string) string {
	return filepath.Join(herdPath, ".claude", "worktrees", slug)
}

// GatherTddGroundTruth reads the state a finished tdd session left behind. The
// sandbox commits into the shared `.git` (bind-mounted), so the feature branch
// ref + objects are visible here without touching the worktree itself — the
// harness never runs git inside a worktree (whose `.git` pointer is container-
// relative), only against the main checkout.
func GatherTddGroundTruth(herdPath, slug string) verify.GroundTruth {
	_, statErr := os.Stat(WorktreePath(herdPath, slug))
	worktreeExists := statErr == nil

	commitsAhead := 0
	// On any failure (branch doesn't exist / no upstream) treat as zero ahead.
	out, err := exec.Command(
		"git", "-C", herdPath, "rev-list", "--count", "origin/main..feat/"+slug,
	).Output()
	if err == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(string(out))); perr == nil {
			commitsAhead = n
		}
	}

	return verify.GroundTruth{WorktreeExists: worktreeExists, CommitsAhead: commitsAhead}
}

// BranchPushed reports whether `feat/<slug>` reached origin, read from the main
// checkout's remote-tracking ref (the review tool's host-side push sets it). It
// is the safe gate on tearing down a worktree: the harness only removes a
// worktree whose branch is on the remote, so a teardown can never lose work
// that hasn't been pushed (DESIGN.md "On a clean run … git worktree remove").
func BranchPushed(herdPath, slug string) bool {
	err := exec.Command(
		"git", "-C", herdPath, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/feat/"+slug,
	).Run()
	return err == nil
}

// RemoveWorktree tears down the worktree at `.claude/worktrees/<slug>` from the
// main checkout. The real-path bind mount (ADR-0002) makes the worktree's
// absolute `.git` pointer resolve on the host, so no throwaway container is
// needed. It is deliberately not forced: if the worktree still holds
// uncommitted work, git refuses and the harness keeps it (a recoverable
// artifact) rather than nuking unpushed changes.
func RemoveWorktree(herdPath, slug string) error {
	return exec.Command(
		"git", "-C", herdPath, "worktree", "remove", WorktreePath(herdPath, slug),
	).Run()
}
