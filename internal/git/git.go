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

// BranchName is the deterministic feature branch the tdd skill creates for a slug.
func BranchName(slug string) string {
	return "feat/" + slug
}

// FetchMain fast-forwards the primary checkout's view of origin/main so commit
// ranges and the PR base are current (DESIGN.md: pull origin/main after every
// session). A fetch failure is returned for the caller to log, not fatal.
func FetchMain(herdPath string) error {
	return exec.Command("git", "-C", herdPath, "fetch", "-q", "origin", "main").Run()
}

// CommitSubjects returns the subject lines of the commits on the feature branch
// ahead of origin/main, newest last — the raw material for the templated PR body.
// Read from the main checkout (the shared `.git` holds the branch's objects); an
// empty result on any git failure keeps the caller crash-free.
func CommitSubjects(herdPath, slug string) []string {
	out, err := exec.Command(
		"git", "-C", herdPath, "log", "--reverse", "--format=%s", "origin/main.."+BranchName(slug),
	).Output()
	if err != nil {
		return nil
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			subjects = append(subjects, s)
		}
	}
	return subjects
}

// WorktreeClean reports whether the worktree has no uncommitted changes — the
// guarantee that the tree the host-side gate validated is exactly the tree that
// `Push` ships. The gate runs against the worktree's working files (committed +
// uncommitted), but the push ships only the committed branch tip; a dirty worktree
// (e.g. a review session that edited but never committed) would mean the gate
// validated a different tree than would ship. Read host-side via the real-path
// mount; any git failure is treated as not-clean (fail safe — never push on doubt).
func WorktreeClean(worktreePath string) bool {
	out, err := exec.Command("git", "-C", worktreePath, "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == ""
}

// Push pushes the feature branch to origin from the main checkout (ADR-0002: the
// harness owns the push, host-side; the sandbox never reaches a remote). Run only
// after the harness's own gate re-run is green.
func Push(herdPath, slug string) error {
	return exec.Command("git", "-C", herdPath, "push", "origin", BranchName(slug)).Run()
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
