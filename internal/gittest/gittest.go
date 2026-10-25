// Package gittest builds real git repositories for tests: a Consumer checkout
// over a local bare origin, and the ticket-worktree states the Stages act on — a
// handoff commit, uncommitted edits, a sibling's merge on main, a disjoint
// history.
//
// It is the git half of hostio.Fake (#28, ADR-0025). Git is local, so a test does
// not need a scripted double of it: a state built here is one git really holds,
// every read the harness makes of it is the production read, and a state change a
// Stage causes — a rebase that collapses the branch, a regraft that heals it, a
// push that reaches origin — happens in git rather than in a flag a test flips.
//
// Like net/http/httptest it is in the non-test build so every package's tests
// can import it; nothing outside a _test.go file should.
package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
)

// Prefix is the branch prefix every [Repo] is opened with: a ticket's branch is
// `feat/<slug>`.
const Prefix = "feat"

// HandoffSubject is the subject of the commit [Repo.Handoff] leaves on a branch.
const HandoffSubject = "feat: do the thing"

// Repo is a Consumer checkout, on main, whose origin is a bare repository in the
// same temp dir. A second clone stands in for everyone else who pushes to that
// origin, so main can move underneath the checkout until it fetches.
type Repo struct {
	t testing.TB
	// Dir is the Consumer checkout: the path a config's project_path names.
	Dir string
	// Origin is the bare repository the checkout fetches from and pushes to.
	Origin string

	others string
}

// New builds a checkout with one commit on main, pushed to its origin.
func New(t testing.TB) *Repo {
	t.Helper()
	root := t.TempDir()
	r := &Repo{t: t, Dir: filepath.Join(root, "checkout"), Origin: filepath.Join(root, "origin.git"), others: filepath.Join(root, "others")}
	r.Git(root, "init", "-q", "--bare", "-b", "main", r.Origin)
	r.Git(root, "init", "-q", "-b", "main", r.Dir)
	identify(r, r.Dir)
	r.Git(r.Dir, "remote", "add", "origin", r.Origin)
	r.write(r.Dir, "README.md", "a consumer\n")
	r.Git(r.Dir, "add", "-A")
	r.Git(r.Dir, "commit", "-q", "-m", "init")
	r.Git(r.Dir, "push", "-q", "-u", "origin", "main")
	r.Git(root, "clone", "-q", r.Origin, r.others)
	identify(r, r.others)
	return r
}

// identify gives a repository the commit identity and settings a test machine's
// global config may not: no signing, no hooks reaching outside the temp dir.
func identify(r *Repo, dir string) {
	r.Git(dir, "config", "user.name", "Test")
	r.Git(dir, "config", "user.email", "test@example.com")
	r.Git(dir, "config", "commit.gpgsign", "false")
}

// Checkout is the harness's view of the checkout: what hostio builds from a
// Consumer's project_path and branch_prefix.
func (r *Repo) Checkout() gitpkg.Checkout { return gitpkg.Open(r.Dir, Prefix) }

// Worktree names slug's worktree. Like [gitpkg.Checkout.Worktree] it creates
// nothing.
func (r *Repo) Worktree(slug string) gitpkg.Worktree { return r.Checkout().Worktree(slug) }

// Handoff is the state an implementation session hands review: slug's worktree,
// created from origin/main, carrying one commit that changes a file.
func (r *Repo) Handoff(slug string) gitpkg.Worktree {
	r.t.Helper()
	wt := r.Worktree(slug)
	if err := wt.Create(); err != nil {
		r.t.Fatalf("create worktree %s: %v", slug, err)
	}
	r.Commit(wt, slug+".txt", "the fix\n", HandoffSubject)
	return wt
}

// Commit writes body to file in wt and commits it, as a session would.
func (r *Repo) Commit(wt gitpkg.Worktree, file, body, subject string) {
	r.t.Helper()
	r.write(wt.Path(), file, body)
	r.Git(wt.Path(), "add", "-A")
	r.Git(wt.Path(), "commit", "-q", "-m", subject)
}

// Dirty leaves an uncommitted edit in wt, as a session killed mid-edit would.
func (r *Repo) Dirty(wt gitpkg.Worktree) {
	r.t.Helper()
	r.write(wt.Path(), "uncommitted.txt", "half a fix\n")
}

// Merge lands a commit writing body to file on origin's main from another clone,
// as a sibling PR merging would. The checkout does not see it until it fetches.
func (r *Repo) Merge(file, body, subject string) {
	r.t.Helper()
	r.Git(r.others, "pull", "-q", "--ff-only", "origin", "main")
	r.write(r.others, file, body)
	r.Git(r.others, "add", "-A")
	r.Git(r.others, "commit", "-q", "-m", subject)
	r.Git(r.others, "push", "-q", "origin", "main")
}

// Conflict lands a sibling's different edit to the file [Repo.Handoff] changed for
// slug, so slug's branch no longer replays cleanly onto origin/main.
func (r *Repo) Conflict(slug string) {
	r.t.Helper()
	r.Merge(slug+".txt", "a different fix\n", "a sibling's different fix")
}

// Supersede lands the very change [Repo.Handoff] made for slug, as a sibling PR
// fixing the same thing would: replayed onto origin/main, slug's branch collapses
// to nothing.
func (r *Repo) Supersede(slug string) {
	r.t.Helper()
	r.Merge(slug+".txt", "the fix\n", "a sibling landed the same fix")
}

// Resolve brings wt onto origin/main by merging it, keeping the branch's side of
// any conflict — what a conflict-resolution session leaves behind.
func (r *Repo) Resolve(wt gitpkg.Worktree) {
	r.t.Helper()
	r.Git(wt.Path(), "merge", "-q", "-X", "ours", "--no-edit", "origin/main")
}

// Publish pushes branch from another clone, as a human opening a PR from it would:
// a branch off main with one commit, which the checkout has never fetched. It
// returns the branch's tip.
func (r *Repo) Publish(branch string) string {
	r.t.Helper()
	r.Git(r.others, "pull", "-q", "--ff-only", "origin", "main")
	r.Git(r.others, "checkout", "-q", "-b", branch)
	r.write(r.others, strings.ReplaceAll(branch, "/", "-")+".txt", "a change\n")
	r.Git(r.others, "add", "-A")
	r.Git(r.others, "commit", "-q", "-m", "a change on "+branch)
	r.Git(r.others, "push", "-q", "origin", branch)
	tip := r.Git(r.others, "rev-parse", "HEAD")
	r.Git(r.others, "checkout", "-q", "main")
	return tip
}

// Disjoint re-roots wt's branch on a history unrelated to main, keeping one commit
// of real work — the BEH-355 shape, where a session's handoff landed on a branch
// that shares no ancestor with origin/main.
func (r *Repo) Disjoint(wt gitpkg.Worktree) {
	r.t.Helper()
	r.Git(wt.Path(), "checkout", "-q", "--orphan", "gittest-orphan")
	r.Git(wt.Path(), "rm", "-rfq", ".")
	r.write(wt.Path(), "README.md", "an unrelated root\n")
	r.write(wt.Path(), "trapped.txt", "verified work\n")
	r.Git(wt.Path(), "add", "-A")
	r.Git(wt.Path(), "commit", "-q", "-m", "work on an unrelated root")
	r.Git(wt.Path(), "branch", "-q", "-M", wt.Branch())
}

// BlockWorktrees makes every worktree uncreatable: a file where the worktrees
// directory belongs, so `git worktree add` fails as it would on a full disk.
// Unblock reverses it.
func (r *Repo) BlockWorktrees() {
	r.t.Helper()
	r.write(r.Dir, filepath.Join(".claude", "worktrees"), "not a directory\n")
}

// Unblock removes what BlockWorktrees put in the way.
func (r *Repo) Unblock() {
	r.t.Helper()
	if err := os.Remove(filepath.Join(r.Dir, ".claude", "worktrees")); err != nil {
		r.t.Fatalf("unblock worktrees: %v", err)
	}
}

// Subject is the subject of the commit at wt's HEAD.
func (r *Repo) Subject(wt gitpkg.Worktree) string {
	r.t.Helper()
	return r.Git(wt.Path(), "log", "-1", "--format=%s")
}

// OnOrigin is branch's tip on origin, or "" when origin does not have it.
func (r *Repo) OnOrigin(branch string) string {
	r.t.Helper()
	out, err := exec.Command("git", "-C", r.Origin, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Git runs git in dir and returns its trimmed stdout, failing the test on error.
func (r *Repo) Git(dir string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

func (r *Repo) write(dir, file, body string) {
	r.t.Helper()
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatalf("mkdir for %s: %v", file, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		r.t.Fatalf("write %s: %v", file, err)
	}
}
