// Package git gathers the ground truth a finished tdd session leaves behind,
// read from the host's primary checkout.
package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/verify"
)

// commandRunner runs a command to completion, returning only its error.
// Production uses execRun; tests inject a fake to drive the retry logic
// without touching a real remote (mirrors sandbox.Preflight's runner seam).
type commandRunner func(name string, args ...string) error

func execRun(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// remoteAttempts is how many times a git remote op (fetch/push) is tried before
// giving up. Transient remote/auth blips — a momentary keychain lock, a network
// hiccup — surface as `exit status 128`; a finished, gate-green worktree must
// not be stranded by one of them (BEH-329 post-mortem: a single transient 128
// on fetch+push aborted the pipeline and left the branch unpushed).
const remoteAttempts = 3

// remoteRetryDelay is the backoff between remote attempts. A var so tests can
// drop it to zero.
var remoteRetryDelay = 2 * time.Second

// withRetry runs op up to remoteAttempts times, sleeping remoteRetryDelay
// between tries; returns nil on the first success, else the last error. It does
// not sleep after the final attempt.
func withRetry(op func() error, sleep func(time.Duration)) error {
	var err error
	for attempt := 1; attempt <= remoteAttempts; attempt++ {
		if err = op(); err == nil {
			return nil
		}
		if attempt < remoteAttempts {
			sleep(remoteRetryDelay)
		}
	}
	return err
}

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
// session). Retried against transient remote blips; a fetch failure is returned
// for the caller to log, not fatal.
func FetchMain(herdPath string) error {
	return fetchMain(herdPath, execRun, time.Sleep)
}

func fetchMain(herdPath string, run commandRunner, sleep func(time.Duration)) error {
	return withRetry(func() error {
		return run("git", "-C", herdPath, "fetch", "-q", "origin", "main")
	}, sleep)
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
//
// --no-verify deliberately skips the host lefthook pre-push hook: the harness has
// already independently re-run the full gate in a throwaway Linux container
// (review/main.go BuildGateRunArgs) — that container's exit code is the sole
// authority for a push. The host hook is redundant duplication, and running it
// here is actively wrong: the push happens from the main checkout (HEAD=main, not
// the feature branch), so lefthook either silently skips every command (its
// push-file set is empty) or, if it ran, would build the wrong tree against
// host-platform node_modules the worktree doesn't have. Retried against transient
// remote blips (BEH-329).
func Push(herdPath, slug string) error {
	return push(herdPath, slug, execRun, time.Sleep)
}

func push(herdPath, slug string, run commandRunner, sleep func(time.Duration)) error {
	return withRetry(func() error {
		return run("git", "-C", herdPath, "push", "--no-verify", "origin", BranchName(slug))
	}, sleep)
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
