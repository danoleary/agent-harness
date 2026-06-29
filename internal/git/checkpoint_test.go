package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs a git command in dir and fails the test on error — test setup only.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// lastCommitSubject reads the subject of the worktree's current HEAD commit.
func lastCommitSubject(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("read HEAD subject: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// commitCount returns how many commits HEAD has in the worktree.
func commitCount(t *testing.T, dir string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	if err != nil {
		t.Fatalf("count commits: %v", err)
	}
	n := 0
	for _, c := range strings.TrimSpace(string(out)) {
		n = n*10 + int(c-'0')
	}
	return n
}

// newRepoWithWorktree creates a real git repo with one commit on main, then adds
// a worktree on a fresh feature branch — the shape a tdd session leaves behind.
// It returns (repoRoot, worktreePath).
func newRepoWithWorktree(t *testing.T, slug string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	wt := filepath.Join(repo, "wt")
	runGit(t, repo, "worktree", "add", "-q", "-b", BranchName(slug), wt)
	return repo, wt
}

// BEH-479: a tdd session that hits the wall-clock cap (or refuses) before reaching
// its own handoff commit leaves the finished diff uncommitted in the worktree,
// which the harness used to merely warn about — the work then needed manual
// recovery. CheckpointCommit captures it as a real commit on the feature branch so
// the diff is recoverable, not discarded.
func TestCheckpointCommitCapturesUncommittedWork(t *testing.T) {
	_, wt := newRepoWithWorktree(t, "beh-479")

	// The session leaves uncommitted work (both a new file and a tracked edit).
	feat := filepath.Join(wt, "src", "feature.ts")
	if err := os.MkdirAll(filepath.Dir(feat), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(feat, []byte("export const x = 1\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	before := commitCount(t, wt)
	if err := CheckpointCommit(wt, "BEH-479", "tdd"); err != nil {
		t.Fatalf("CheckpointCommit: %v", err)
	}

	// The work is now committed (recoverable) and the worktree is clean.
	if !WorktreeClean(wt) {
		t.Fatalf("worktree should be clean after the checkpoint captured the diff")
	}
	if got := commitCount(t, wt); got != before+1 {
		t.Fatalf("branch should advance by exactly one checkpoint commit: before=%d after=%d", before, got)
	}
	// The untracked new file was captured too (this is recovery, capture everything).
	if err := exec.Command("git", "-C", wt, "cat-file", "-e", "HEAD:src/feature.ts").Run(); err != nil {
		t.Fatalf("the new untracked file should be in the checkpoint commit: %v", err)
	}
}

// commitIdentity reads "author-name|author-email|committer-name|committer-email"
// of the worktree's HEAD commit — the four fields the placeholder leaks into.
func commitIdentity(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%an|%ae|%cn|%ce").Output()
	if err != nil {
		t.Fatalf("read HEAD identity: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// BEH-579: the checkpoint commit is made host-side against a worktree whose repo
// config may carry the placeholder `Test <test@example.com>` identity. It must
// stamp the harness bot identity (author AND committer) so a recovery commit that
// reaches PR history doesn't pollute `git blame`/contributor stats — even though
// the surrounding repo config is the placeholder.
func TestCheckpointCommitStampsHarnessIdentity(t *testing.T) {
	_, wt := newRepoWithWorktree(t, "beh-579") // seeded with the Test placeholder config

	feat := filepath.Join(wt, "feature.ts")
	if err := os.WriteFile(feat, []byte("export const x = 1\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := CheckpointCommit(wt, "BEH-579", "tdd"); err != nil {
		t.Fatalf("CheckpointCommit: %v", err)
	}

	want := HarnessAuthorName + "|" + HarnessAuthorEmail + "|" + HarnessAuthorName + "|" + HarnessAuthorEmail
	if got := commitIdentity(t, wt); got != want {
		t.Fatalf("checkpoint identity = %q, want %q (the placeholder Test identity must not leak)", got, want)
	}
}

// A clean worktree means the session already handed off (or produced nothing) —
// there is nothing to recover, so the checkpoint must not manufacture an empty
// commit that would pollute the branch and read as work that does not exist.
func TestCheckpointCommitNoOpWhenClean(t *testing.T) {
	_, wt := newRepoWithWorktree(t, "beh-479")

	before := commitCount(t, wt)
	if err := CheckpointCommit(wt, "BEH-479", "tdd"); err != nil {
		t.Fatalf("CheckpointCommit on a clean worktree should be a no-op success, got %v", err)
	}

	if got := commitCount(t, wt); got != before {
		t.Fatalf("a clean worktree must not gain a commit: before=%d after=%d", before, got)
	}
	if subj := lastCommitSubject(t, wt); strings.Contains(subj, "checkpoint") {
		t.Fatalf("no checkpoint commit should exist; HEAD subject = %q", subj)
	}
}

// The checkpoint's subject must mark it as a harness recovery commit and name the
// ticket, so a reviewer (or a future verify/recovery step) never mistakes a
// salvaged-on-timeout diff for a real, verified handoff.
func TestCheckpointMessageMarksHarnessRecoveryAndTicket(t *testing.T) {
	msg := CheckpointMessage("BEH-479", "tdd")

	subject := strings.SplitN(msg, "\n", 2)[0]
	if !strings.Contains(subject, "checkpoint") {
		t.Fatalf("subject must announce a checkpoint, got %q", subject)
	}
	if !strings.Contains(subject, "harness") {
		t.Fatalf("subject must attribute the commit to the harness, got %q", subject)
	}
	if !strings.Contains(subject, "BEH-479") {
		t.Fatalf("subject must name the ticket, got %q", subject)
	}
	if !strings.Contains(msg, "NOT a verified handoff") {
		t.Fatalf("body must warn the work is unverified, got %q", msg)
	}
}

// The checkpoint message must name the *session* that left the work (tdd vs
// review), so a reviewer (and a resumed review) can tell which stage's
// uncommitted edits were salvaged. Before BEH-559 the wording was hardcoded to
// "tdd", which mislabelled a review-stage checkpoint and gave the resuming review
// no signal that a prior review pass had begun a fix.
func TestCheckpointMessageNamesSession(t *testing.T) {
	review := CheckpointMessage("BEH-559", "review")
	if !strings.Contains(review, "review") {
		t.Fatalf("a review checkpoint message must name the review session, got %q", review)
	}

	tdd := CheckpointMessage("BEH-559", "tdd")
	if review == tdd {
		t.Fatalf("checkpoint messages for different sessions must differ; both were %q", review)
	}
}

// BEH-559: a review session killed mid-edit by a spending cap leaves its
// in-progress fixes uncommitted. Without capturing them, the next (resumed)
// review re-derives the diff from the committed tip (git diff merge-base) and
// never sees them — flipping the verdict on the same line. A review-stage
// checkpoint commit must preserve those edits on the branch so the resumed
// review's merge-base diff includes the started fix instead of the original
// clean commit.
func TestCheckpointCommitPreservesReviewEditsForResumedDiff(t *testing.T) {
	_, wt := newRepoWithWorktree(t, "beh-559")

	// The implementation handoff: a committed file on the feature branch — the
	// "original clean commit" the resumed review would otherwise re-judge.
	src := filepath.Join(wt, "runner.ts")
	if err := os.WriteFile(src, []byte("useRef(createLatestWinsRunner())\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-q", "-m", "feat: handoff (BEH-559)")

	// review-1 begins applying the lazy-init nit fix, then is cap-killed mid-edit:
	// the change is on disk but uncommitted.
	if err := os.WriteFile(src, []byte("useRef(() => createLatestWinsRunner())\n"), 0o644); err != nil {
		t.Fatalf("review edit: %v", err)
	}

	if err := CheckpointCommit(wt, "BEH-559", "review"); err != nil {
		t.Fatalf("CheckpointCommit: %v", err)
	}

	// What review-2 (the resume) re-derives: the diff from the merge-base with
	// main. It must now contain review-1's in-progress fix, not the original line
	// alone — so the resumed review sees the started work rather than flipping the
	// verdict on a "clean" diff.
	baseOut, err := exec.Command("git", "-C", wt, "merge-base", "HEAD", "main").Output()
	if err != nil {
		t.Fatalf("merge-base: %v", err)
	}
	diff, err := exec.Command("git", "-C", wt, "diff", strings.TrimSpace(string(baseOut))+"..HEAD").Output()
	if err != nil {
		t.Fatalf("resume diff: %v", err)
	}
	if !strings.Contains(string(diff), "() => createLatestWinsRunner()") {
		t.Fatalf("resumed review's merge-base diff must include the preserved in-progress fix, got:\n%s", diff)
	}
}

// The checkpoint stages everything (`-A`), excludes the readiness sentinel
// (BEH-612), and commits `--no-verify` against the worktree — `--no-verify` because
// the salvaged work may not pass hooks (that is why it is a checkpoint, not a
// handoff), mirroring Push's reasoning. The all-success scripted runner can't model
// the staged-diff probe (its exit code is the boolean), so this runner returns a
// non-zero exit for `git diff --cached --quiet` = "real work is staged".
func TestCheckpointCommitStagesExcludesSentinelAndSkipsHooks(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if containsArg(args, "diff") {
			return errors.New("exit status 1") // staged changes exist
		}
		return nil
	}
	if err := checkpointCommit("/wt", "msg", run); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if len(calls) != 4 {
		t.Fatalf("expected stage, unstage-sentinel, probe, commit — got %d calls: %v", len(calls), calls)
	}
	if add := strings.Join(calls[0], " "); add != "git -C /wt add -A" {
		t.Fatalf("first call should stage everything, got %q", add)
	}
	if reset := strings.Join(calls[1], " "); !strings.Contains(reset, "reset") || !strings.Contains(reset, WorktreeReadySentinel) {
		t.Fatalf("second call should unstage the readiness sentinel, got %q", reset)
	}
	if probe := strings.Join(calls[2], " "); !strings.Contains(probe, "diff") || !strings.Contains(probe, "--cached") {
		t.Fatalf("third call should probe the staged diff, got %q", probe)
	}
	commit := strings.Join(calls[3], " ")
	if !strings.Contains(commit, "-C /wt") || !strings.Contains(commit, "commit") {
		t.Fatalf("fourth call should commit in the worktree, got %q", commit)
	}
	if !strings.Contains(commit, "--no-verify") {
		t.Fatalf("commit must skip hooks, got %q", commit)
	}
}

// containsArg reports whether argv contains the exact token — used to spot the
// `diff` / `commit` calls in a scripted checkpoint runner.
func containsArg(argv []string, token string) bool {
	for _, a := range argv {
		if a == token {
			return true
		}
	}
	return false
}

// Belt-and-braces (BEH-612): even if the dirty-tree guard let it through, the
// checkpoint must refuse to commit when nothing real is staged — once the sentinel
// is excluded the index can be empty, and a checkpoint must never capture zero work
// (it would pollute the branch and, pre-fix, reattach the gitignored sentinel). An
// all-success runner makes `git diff --cached --quiet` report "no staged changes".
func TestCheckpointCommitRefusesEmptyStagedCommit(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	if err := checkpointCommit("/wt", "msg", run); err != nil {
		t.Fatalf("expected a no-op success, got %v", err)
	}
	for _, c := range *calls {
		if containsArg(c, "commit") {
			t.Fatalf("must not commit when nothing real is staged, calls: %v", *calls)
		}
	}
}

// BEH-612: a recovery checkpoint must never capture the readiness sentinel. When
// the sentinel is the ONLY uncommitted change the checkpoint is a no-op (no real
// work to recover); when real work sits alongside it, the checkpoint commits the
// work but leaves the sentinel out of the tree — so a PR can never ship it.
func TestCheckpointCommitExcludesReadySentinel(t *testing.T) {
	_, wt := newRepoWithWorktree(t, "beh-612")

	if err := os.WriteFile(filepath.Join(wt, WorktreeReadySentinel), nil, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	before := commitCount(t, wt)
	if err := CheckpointCommit(wt, "BEH-612", "review"); err != nil {
		t.Fatalf("CheckpointCommit: %v", err)
	}
	if got := commitCount(t, wt); got != before {
		t.Fatalf("a sentinel-only worktree must not gain a checkpoint commit: before=%d after=%d", before, got)
	}

	// Real work alongside the sentinel → the work is captured, the sentinel is not.
	if err := os.WriteFile(filepath.Join(wt, "real.ts"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := CheckpointCommit(wt, "BEH-612", "review"); err != nil {
		t.Fatalf("CheckpointCommit: %v", err)
	}
	if got := commitCount(t, wt); got != before+1 {
		t.Fatalf("real work must produce exactly one checkpoint commit: before=%d after=%d", before, got)
	}
	if err := exec.Command("git", "-C", wt, "cat-file", "-e", "HEAD:real.ts").Run(); err != nil {
		t.Fatalf("the real work should be in the checkpoint commit: %v", err)
	}
	if err := exec.Command("git", "-C", wt, "cat-file", "-e", "HEAD:"+WorktreeReadySentinel).Run(); err == nil {
		t.Fatalf("the readiness sentinel must NOT be committed into the checkpoint")
	}
}

// A failed `git add` must abort before committing and surface the error, so the
// caller knows the diff was not captured and can warn for manual recovery rather
// than silently believing the work is safe.
func TestCheckpointCommitStopsAndReportsWhenStagingFails(t *testing.T) {
	run, calls := scriptedRunner(1, errors.New("exit status 128"))
	err := checkpointCommit("/wt", "msg", run)
	if err == nil {
		t.Fatalf("expected the staging failure to propagate")
	}
	if len(*calls) != 1 {
		t.Fatalf("commit must not be attempted after a failed stage, got calls: %v", *calls)
	}
}
