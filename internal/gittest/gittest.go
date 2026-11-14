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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// Consumer's project_path and branch_prefix, with two differences. A remote op is
// tried once: the origin is a local bare repository, so a failure is never a blip
// to retry, and a test that makes a fetch or push fail would otherwise wait out
// the real multi-minute retry budget (#254). And host git runs the checkout's
// hooks, which production git never does (#332): [Repo.InTheWayOfReplay] and
// [Repo.RefuseReplayedCommit] make git fail through one.
func (r *Repo) Checkout() gitpkg.Checkout {
	return gitpkg.Open(r.Dir, Prefix).WithoutRemoteRetry().WithRepoHooks()
}

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
	return r.PublishFile(branch, strings.ReplaceAll(branch, "/", "-")+".txt", "a change\n")
}

// PublishFile is [Repo.Publish] with the branch's one commit writing body to file.
func (r *Repo) PublishFile(branch, file, body string) string {
	r.t.Helper()
	r.Git(r.others, "pull", "-q", "--ff-only", "origin", "main")
	r.Git(r.others, "checkout", "-q", "-b", branch)
	r.write(r.others, file, body)
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

// InTheWayOfReplay makes the next `times` replays of wt's branch fail with no pick
// left in progress, as a transient host-side git failure does (#164). After each of
// the next `times` commits git makes in the checkout, a post-commit hook leaves an
// untracked file in wt where the commit after it wants to write: the replay's
// cherry-pick commits one pick and then refuses the next ("untracked working tree
// files would be overwritten"), and its abort-and-restore puts the branch back.
// file must be one that a later commit on the branch adds.
func (r *Repo) InTheWayOfReplay(wt gitpkg.Worktree, file string, times int) {
	r.t.Helper()
	r.hook("post-commit", times, fmt.Sprintf("spend\necho stray > %q", filepath.Join(wt.Path(), file)))
}

// RefuseReplayedCommit makes git refuse, the next `times` times, to move wt's
// branch to a commit whose subject is subject: a reference-transaction hook rejects
// the update, so a cherry-pick of that commit halts with its change staged, no
// unmerged paths and the pick still in progress — a halt that is neither an empty
// commit nor a content conflict (#164).
func (r *Repo) RefuseReplayedCommit(wt gitpkg.Worktree, subject string, times int) {
	r.t.Helper()
	r.hook("reference-transaction", times, fmt.Sprintf(`[ "$1" = prepared ] || exit 0
refused=
while read -r old new ref; do
	[ "$ref" = %q ] || continue
	[ "$(git log -1 --format=%%s "$new" 2>/dev/null)" = %q ] && refused=1
done
[ -n "$refused" ] || exit 0
spend
echo "the branch update is refused" >&2
exit 1`, "refs/heads/"+wt.Branch(), subject))
}

// hook installs body as the checkout's name hook, live for `times` runs: body
// calls spend when it acts, and once none are left the hook does nothing. The
// hooks live in a directory of their own, set as core.hooksPath, so a test
// machine's global hooksPath can neither replace nor add to them.
func (r *Repo) hook(name string, times int, body string) {
	r.t.Helper()
	dir := filepath.Join(filepath.Dir(r.Dir), "hooks")
	counter := filepath.Join(dir, name+".left")
	r.write(dir, name+".left", strconv.Itoa(times)+"\n")
	r.write(dir, name, fmt.Sprintf(`#!/bin/sh
left=$(cat %[1]q 2>/dev/null || echo 0)
[ "$left" -gt 0 ] || exit 0
spend() { echo $((left - 1)) > %[1]q; }
%[2]s
`, counter, body))
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		r.t.Fatalf("chmod %s hook: %v", name, err)
	}
	r.Git(r.Dir, "config", "core.hooksPath", dir)
}
