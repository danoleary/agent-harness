package git

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Remove tears the worktree down from the MAIN checkout (`git -C <repo> worktree
// remove <path>`), and deliberately without `--force`: a worktree still holding
// uncommitted work makes git refuse, and the harness would rather keep a
// recoverable artifact than nuke unpushed changes. Before the Worktree type this
// was one of the exported halves no test could reach.
func TestRemoveRunsFromTheCheckoutAndIsNotForced(t *testing.T) {
	run, calls := recordingRunner(func([]string) error { return nil })
	wt := fakeWorktree("beh-700", runners{run: run})
	if err := wt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one git call, got %v", *calls)
	}
	got := (*calls)[0]
	want := []string{"git", "-C", testRepo, "worktree", "remove", wt.Path()}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if hasArg(got, "--force") {
		t.Error("teardown must NOT be forced — git refusing on uncommitted work is the safety net")
	}
}

// A refused teardown (the worktree holds uncommitted work) is surfaced, not
// swallowed: the caller keeps the worktree and says so rather than believing it
// reclaimed the disk.
func TestRemovePropagatesGitRefusal(t *testing.T) {
	run, _ := recordingRunner(func([]string) error {
		return errors.New("fatal: '…' contains modified or untracked files, use --force to delete it")
	})
	if err := fakeWorktree("beh-700", runners{run: run}).Remove(); err == nil {
		t.Fatal("expected the refusal to propagate")
	}
}

// CommitSubjects is the raw material for the templated PR body. It reads the
// branch's commits ahead of origin/main from the MAIN checkout — the shared `.git`
// holds the branch's objects, so the worktree need not even exist — newest last.
func TestCommitSubjectsReadsBranchAheadOfMainFromTheCheckout(t *testing.T) {
	var argv []string
	out := func(name string, args ...string) ([]byte, error) {
		argv = append([]string{name}, args...)
		return []byte("feat: first\nfix: second\n"), nil
	}
	wt := fakeWorktree("beh-700", runners{output: out})
	got := wt.CommitSubjects()
	if want := []string{"feat: first", "fix: second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subjects = %v, want %v (newest last, in order)", got, want)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-C "+testRepo) {
		t.Errorf("the log must be read from the main checkout, got %q", joined)
	}
	if !strings.Contains(joined, "origin/main.."+wt.Branch()) {
		t.Errorf("the range must be origin/main..<branch>, got %q", joined)
	}
	if !strings.Contains(joined, "--reverse") {
		t.Errorf("--reverse is what puts the newest commit last, got %q", joined)
	}
}

// Blank lines in the log output are dropped, so a stray newline never becomes an
// empty bullet in the PR body.
func TestCommitSubjectsSkipsBlankLines(t *testing.T) {
	out := func(string, ...string) ([]byte, error) { return []byte("feat: one\n\n  \nfix: two\n"), nil }
	got := fakeWorktree("beh-700", runners{output: out}).CommitSubjects()
	if want := []string{"feat: one", "fix: two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("subjects = %v, want %v", got, want)
	}
}

// Any git failure (an absent branch, no upstream) yields no subjects rather than a
// crash: the PR body degrades to its template, which is not worth failing a ship
// over.
func TestCommitSubjectsEmptyOnGitError(t *testing.T) {
	out := func(string, ...string) ([]byte, error) { return nil, errors.New("exit status 128") }
	if got := fakeWorktree("beh-700", runners{output: out}).CommitSubjects(); got != nil {
		t.Errorf("subjects = %v, want nil on a git error", got)
	}
}

// BranchExists and BranchPushed are a deliberate pair over two DIFFERENT refs:
// BranchExists reads the LOCAL head (where the sandbox's commits land through the
// shared `.git`), BranchPushed the REMOTE-TRACKING ref (set by the host's own
// push). Confusing them would let the worktree reaper tear down a worktree whose
// work never reached origin, so pin which ref each one reads.
func TestBranchExistsAndBranchPushedReadDifferentRefs(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(Worktree) bool
		ref  string
	}{
		{"BranchExists reads the local head", Worktree.BranchExists, "refs/heads/"},
		{"BranchPushed reads the remote-tracking ref", Worktree.BranchPushed, "refs/remotes/origin/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, calls := recordingRunner(func([]string) error { return nil })
			wt := fakeWorktree("beh-553", runners{run: run})
			if !tc.call(wt) {
				t.Fatal("a resolving ref must report true")
			}
			joined := strings.Join((*calls)[0], " ")
			if !strings.Contains(joined, tc.ref+wt.Branch()) {
				t.Errorf("must probe %q, got %q", tc.ref+wt.Branch(), joined)
			}
			if !strings.Contains(joined, "-C "+testRepo) {
				t.Errorf("the ref must be read from the main checkout, got %q", joined)
			}
		})
	}
}

// An unresolvable ref (git exits non-zero) reads as absent for both — the
// fail-safe direction: the retrospective skips a branch it cannot see, and the
// reaper keeps a worktree it cannot prove was pushed.
func TestBranchExistsAndBranchPushedFalseWhenRefUnresolvable(t *testing.T) {
	run, _ := recordingRunner(func([]string) error { return errors.New("exit status 1") })
	wt := fakeWorktree("beh-553", runners{run: run})
	if wt.BranchExists() {
		t.Error("BranchExists must read an unresolvable ref as absent")
	}
	if wt.BranchPushed() {
		t.Error("BranchPushed must read an unresolvable ref as not-pushed")
	}
}

// Exists is the implementation stage's "already provisioned?" check and the review
// stage's precondition, and it is the first read verify.Tdd spends — a false here
// short-circuits the whole verdict to "worktree was not created". It reads the
// directory, not git, so it answers for a path that never existed.
func TestExistsReadsTheWorktreeDirectory(t *testing.T) {
	dir := t.TempDir()
	if !(Worktree{path: dir}).Exists() {
		t.Error("a worktree directory that is on disk must read as existing")
	}
	if (Worktree{path: filepath.Join(dir, "never-provisioned")}).Exists() {
		t.Error("a worktree path that was never created must read as absent")
	}
}

// CommitsAhead reads the branch's commit count from the MAIN checkout — the shared
// `.git` holds the sandbox's commits, so the worktree need not exist — and reads as
// zero on ANY failure. Zero is what verify.Tdd fails as "no handoff commit", so an
// unresolvable branch or unparseable output must degrade to that rather than to a
// number the push gate would trust.
func TestCommitsAheadReadsFromTheCheckoutAndFailsToZero(t *testing.T) {
	var argv []string
	wt := fakeWorktree("beh-700", runners{output: func(name string, args ...string) ([]byte, error) {
		argv = append([]string{name}, args...)
		return []byte("3\n"), nil
	}})
	if got := wt.CommitsAhead(); got != 3 {
		t.Errorf("CommitsAhead = %d, want 3", got)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "-C "+testRepo) {
		t.Errorf("the count must be read from the main checkout, got %q", joined)
	}
	if !strings.Contains(joined, "origin/main.."+wt.Branch()) {
		t.Errorf("the range must be origin/main..<branch>, got %q", joined)
	}

	failed := fakeWorktree("beh-700", runners{output: func(string, ...string) ([]byte, error) {
		return nil, errors.New("fatal: ambiguous argument 'origin/main..feat/beh-700'")
	}})
	if got := failed.CommitsAhead(); got != 0 {
		t.Errorf("CommitsAhead = %d for an unresolvable branch, want 0 (the no-handoff verdict)", got)
	}

	junk := fakeWorktree("beh-700", runners{output: func(string, ...string) ([]byte, error) {
		return []byte("not a number\n"), nil
	}})
	if got := junk.CommitsAhead(); got != 0 {
		t.Errorf("CommitsAhead = %d for unparseable output, want 0 — never a count the push gate would trust", got)
	}
}
