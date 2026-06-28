package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// execIn runs a git command in dir to completion for test setup (init a repo,
// create a branch). Distinct from the production commandRunner seam — these are
// real git calls against a throwaway repo, the only way to exercise the host-git
// reads (BranchExists) that have no injectable seam.
func execIn(dir string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return cmd.Run()
}

// fakeClock is a deterministic clock for retry tests: time advances only when the
// injected sleeper is called, so the wall-clock backoff/budget logic is exercised
// with zero real waiting (BEH-403). now() and sleep satisfy the withRetry seams.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock              { return &fakeClock{t: time.Unix(0, 0)} }
func (c *fakeClock) now() time.Time         { return c.t }
func (c *fakeClock) sleep(d time.Duration)  { c.t = c.t.Add(d) }
func (c *fakeClock) elapsed() time.Duration { return c.t.Sub(time.Unix(0, 0)) }

// scriptedRunner returns a commandRunner that fails for the first `failures`
// calls (returning errEach) then succeeds, recording every argv it saw.
func scriptedRunner(failures int, errEach error) (commandRunner, *[][]string) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(calls) <= failures {
			return errEach
		}
		return nil
	}
	return run, &calls
}

// blipRunner models a transient remote disruption: it fails while the clock reads
// less than `clearAfter` past its first call, then succeeds — i.e. a blip of a
// given wall-clock duration that the retry loop must ride out.
func blipRunner(clock *fakeClock, clearAfter time.Duration, errEach error) (commandRunner, *int) {
	calls := 0
	start := clock.now()
	run := func(string, ...string) error {
		calls++
		if clock.now().Sub(start) < clearAfter {
			return errEach
		}
		return nil
	}
	return run, &calls
}

// BEH-570: a long pipeline lets sibling PRs merge to main underneath the branch,
// stranding it on a stale base. RebaseOntoMain replays the feature branch onto the
// freshly-fetched origin/main before the push so the PR opens on a current base. A
// clean replay reports RebaseClean and runs exactly one git command (the rebase) —
// no abort.
func TestRebaseOntoMainCleanReportsClean(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	if res := rebaseOntoMain("/wt", run); res != RebaseClean {
		t.Fatalf("res = %v, want RebaseClean", res)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one git call (no abort on a clean rebase), got %d: %v", len(*calls), *calls)
	}
	got := strings.Join((*calls)[0], " ")
	// The rebase carries the harness identity overrides (BEH-579) so replayed
	// commits' committer never inherits the host's placeholder config.
	want := "git -C /wt -c user.name=" + HarnessAuthorName + " -c user.email=" + HarnessAuthorEmail + " rebase origin/main"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

// A rebase that can't be applied cleanly (a genuine content conflict) must abort —
// restoring the branch so it is never left mid-rebase — and report RebaseConflict
// so the caller defers to a human rather than pushing.
func TestRebaseOntoMainConflictAbortsAndReportsConflict(t *testing.T) {
	// First call (the rebase) fails; the second (the abort) succeeds.
	run, calls := scriptedRunner(1, errors.New("exit status 1: CONFLICT (content)"))
	if res := rebaseOntoMain("/wt", run); res != RebaseConflict {
		t.Fatalf("res = %v, want RebaseConflict", res)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected the rebase then an abort, got %d calls: %v", len(*calls), *calls)
	}
	abort := (*calls)[1]
	want := []string{"git", "-C", "/wt", "rebase", "--abort"}
	if len(abort) != len(want) {
		t.Fatalf("abort argv = %v, want %v", abort, want)
	}
	for i := range want {
		if abort[i] != want[i] {
			t.Fatalf("abort argv[%d] = %q, want %q (full: %v)", i, abort[i], want[i], abort)
		}
	}
}

// BEH-412: the implementation tool hands the worktree back to a reviewer who may
// be on a non-Linux host. The sandbox-built `web/node_modules` carries Linux-only
// native bindings that crash the macOS gates, and `pnpm install --frozen-lockfile`
// won't repair them. Stripping the tree forces the reviewer to install fresh for
// their own platform.
// The scripted-runner tests above pin RebaseOntoMain's control flow; this one pins
// the contract it rests on — that real `git rebase origin/main` exits 0 on a clean
// replay and non-zero on a content conflict, and that the abort restores the branch.
// Without it a wrong command name or a git-semantics surprise would pass the fakes
// but silently mislabel every conflict (BEH-570).
func TestRebaseOntoMainAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// setupRepo builds a repo where main has advanced past feat/<slug> by one commit
	// touching `mainFile`, with refs/remotes/origin/main pointing at that advanced tip
	// and feat/<slug> checked out — the exact shape RebaseOntoMain rebases. feat always
	// edits `feat.txt`; whether main's commit also edits `feat.txt` (conflict) or a
	// different file (clean) is the only variable.
	const slug = "beh-570-real"
	setupRepo := func(t *testing.T, mainFile, mainContents string) string {
		t.Helper()
		repo := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			if err := execIn(repo, args...); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
		write := func(name, contents string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		git("init", "-q", "-b", "main")
		git("config", "user.email", "t@example.com")
		git("config", "user.name", "Test")
		write("feat.txt", "base\n")
		git("add", "-A")
		git("commit", "-q", "-m", "base")
		// feat branch edits feat.txt.
		git("checkout", "-q", "-b", BranchName(slug))
		write("feat.txt", "feat change\n")
		git("add", "-A")
		git("commit", "-q", "-m", "feat work")
		// main advances by one commit; origin/main tracks it.
		git("checkout", "-q", "main")
		write(mainFile, mainContents)
		git("add", "-A")
		git("commit", "-q", "-m", "main moved")
		git("update-ref", "refs/remotes/origin/main", "main")
		git("checkout", "-q", BranchName(slug))
		return repo
	}

	t.Run("clean replay onto an advanced base", func(t *testing.T) {
		// main touched a different file, so feat replays cleanly onto the new base.
		repo := setupRepo(t, "unrelated.txt", "main-only\n")
		if res := RebaseOntoMain(repo); res != RebaseClean {
			t.Fatalf("res = %v, want RebaseClean", res)
		}
		// The replayed branch now sits on top of main: it carries main's new file.
		if _, err := os.Stat(filepath.Join(repo, "unrelated.txt")); err != nil {
			t.Fatalf("rebased branch should contain main's commit (unrelated.txt), stat err = %v", err)
		}
		if !WorktreeClean(repo) {
			t.Fatal("worktree should be clean after a successful rebase")
		}
		// BEH-579: a rebase rewrites the COMMITTER of every replayed commit to
		// whoever runs it. Run host-side against a repo carrying the placeholder
		// `Test` config, that would stamp the whole pushed branch's commits as
		// Test. The harness identity must win instead.
		out, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%cn|%ce").Output()
		if err != nil {
			t.Fatalf("read committer: %v", err)
		}
		if got, want := strings.TrimSpace(string(out)), HarnessAuthorName+"|"+HarnessAuthorEmail; got != want {
			t.Fatalf("replayed committer = %q, want %q (the placeholder Test identity must not leak)", got, want)
		}
	})

	t.Run("genuine conflict aborts and restores the branch", func(t *testing.T) {
		// main edited the same file feat did → a real content conflict.
		repo := setupRepo(t, "feat.txt", "main change\n")
		featTip, err := HeadSHA(filepath.Join(repo))
		if err != nil {
			t.Fatalf("read feat tip: %v", err)
		}
		if res := RebaseOntoMain(repo); res != RebaseConflict {
			t.Fatalf("res = %v, want RebaseConflict", res)
		}
		// Aborted cleanly: no rebase in progress, branch back at its original tip.
		if !WorktreeClean(repo) {
			t.Fatal("worktree should be clean after the conflicting rebase was aborted")
		}
		after, err := HeadSHA(repo)
		if err != nil {
			t.Fatalf("read tip after abort: %v", err)
		}
		if after != featTip {
			t.Fatalf("branch tip = %s after abort, want it restored to %s", after, featTip)
		}
	})
}

// BEH-581: after a sandboxed conflict-resolution session, the harness must
// confirm the branch was ACTUALLY rebased onto origin/main before re-gating +
// pushing — a session that gave up and ran `git rebase --abort` leaves a clean
// worktree on the original stale tip, which IsRebasedOnto must catch. It is true
// iff the ref is an ancestor of HEAD (the branch contains the latest base).
func TestIsRebasedOntoTrueWhenAncestor(t *testing.T) {
	// `merge-base --is-ancestor` exits 0 → the ref is an ancestor → rebased.
	run, calls := scriptedRunner(0, nil)
	if !isRebasedOnto("/wt", "origin/main", run) {
		t.Fatal("exit 0 from merge-base --is-ancestor should report rebased (true)")
	}
	got := strings.Join((*calls)[0], " ")
	want := "git -C /wt merge-base --is-ancestor origin/main HEAD"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestIsRebasedOntoFalseWhenNotAncestor(t *testing.T) {
	// A non-zero exit (ref not an ancestor) → NOT rebased.
	run, _ := scriptedRunner(1, errors.New("exit status 1"))
	if isRebasedOnto("/wt", "origin/main", run) {
		t.Fatal("a non-zero merge-base exit should report not-rebased (false)")
	}
}

// Pins the real-git contract IsRebasedOnto rests on: a feat branch replayed onto
// an advanced origin/main reads as rebased; the same branch BEFORE the rebase
// (still on the stale base) reads as not-rebased.
func TestIsRebasedOntoAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if err := execIn(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	const slug = "beh-581-real"
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("base.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", BranchName(slug))
	write("feat.txt", "feat\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat work")
	// main advances on an unrelated file; origin/main tracks it.
	git("checkout", "-q", "main")
	write("unrelated.txt", "main\n")
	git("add", "-A")
	git("commit", "-q", "-m", "main moved")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", BranchName(slug))

	// Before rebasing, origin/main is NOT an ancestor of feat's tip.
	if IsRebasedOnto(repo, "origin/main") {
		t.Fatal("feat on its stale base should NOT read as rebased onto origin/main")
	}
	if res := RebaseOntoMain(repo); res != RebaseClean {
		t.Fatalf("setup rebase = %v, want RebaseClean", res)
	}
	// After a clean replay, origin/main IS an ancestor.
	if !IsRebasedOnto(repo, "origin/main") {
		t.Fatal("after rebasing, feat should read as rebased onto origin/main")
	}
}

// BEH-597: IsDisjointFrom reports whether HEAD shares NO common ancestor with the
// ref — the disjoint-history condition (an empty `git merge-base`) the pre-push
// rebase must catch before mislabelling the inevitable collision a content
// conflict. `git merge-base <ref> HEAD` exits 0 when a common ancestor exists.
func TestIsDisjointFromFalseWhenCommonAncestor(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	if isDisjointFrom("/wt", "origin/main", run) {
		t.Fatal("exit 0 from merge-base (a common ancestor exists) should report NOT disjoint")
	}
	got := strings.Join((*calls)[0], " ")
	want := "git -C /wt merge-base origin/main HEAD"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestIsDisjointFromTrueWhenNoCommonAncestor(t *testing.T) {
	// A non-zero exit (no merge base — disjoint histories) → disjoint.
	run, _ := scriptedRunner(1, errors.New("exit status 1"))
	if !isDisjointFrom("/wt", "origin/main", run) {
		t.Fatal("a non-zero merge-base exit (no common ancestor) should report disjoint (true)")
	}
}

// Pins the real-git contract IsDisjointFrom rests on: two branches built from
// `git init` in separate roots (no shared base) read as disjoint; a normal feature
// branch off main reads as NOT disjoint.
func TestIsDisjointFromAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if err := execIn(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	write := func(name, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("base.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "main")

	// A normal feature branch off main shares main's history → NOT disjoint.
	git("checkout", "-q", "-b", BranchName("beh-597-joined"))
	write("feat.txt", "feat\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat work")
	if IsDisjointFrom(repo, "origin/main") {
		t.Fatal("a feature branch off main shares a common ancestor — must NOT read as disjoint")
	}

	// An orphan branch (built with no parent) shares NO history with main → disjoint.
	git("checkout", "-q", "--orphan", BranchName("beh-597-orphan"))
	write("orphan.txt", "orphan\n")
	git("add", "-A")
	git("commit", "-q", "-m", "orphan root")
	if !IsDisjointFrom(repo, "origin/main") {
		t.Fatal("an orphan branch with no common ancestor must read as disjoint")
	}
}

// BEH-597: GatherTddGroundTruth must surface a disjoint history so verify.Tdd can
// fail it. An orphan feature branch (no common ancestor with origin/main) is the
// BEH-355 condition; a normal feature branch off main is not disjoint.
func TestGatherTddGroundTruthFlagsDisjointHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	mkRepo := func(orphan bool) (string, string) {
		t.Helper()
		repo := t.TempDir()
		git := func(args ...string) {
			t.Helper()
			if err := execIn(repo, args...); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
		write := func(name, contents string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		git("init", "-q", "-b", "main")
		git("config", "user.email", "t@example.com")
		git("config", "user.name", "Test")
		write("base.txt", "base\n")
		git("add", "-A")
		git("commit", "-q", "-m", "base")
		git("update-ref", "refs/remotes/origin/main", "main")
		const slug = "beh-597-gather"
		if orphan {
			git("checkout", "-q", "--orphan", BranchName(slug))
		} else {
			git("checkout", "-q", "-b", BranchName(slug))
		}
		write("feat.txt", "feat\n")
		git("add", "-A")
		git("commit", "-q", "-m", "handoff")
		return repo, slug
	}

	t.Run("orphan branch is disjoint", func(t *testing.T) {
		repo, slug := mkRepo(true)
		truth := GatherTddGroundTruth(repo, slug)
		if !truth.DisjointHistory {
			t.Errorf("an orphan branch (no common ancestor) must be flagged disjoint, got %+v", truth)
		}
		if truth.CommitsAhead < 1 {
			t.Errorf("a disjoint branch is still ahead by its own commits, got CommitsAhead=%d", truth.CommitsAhead)
		}
	})

	t.Run("normal feature branch is not disjoint", func(t *testing.T) {
		repo, slug := mkRepo(false)
		truth := GatherTddGroundTruth(repo, slug)
		if truth.DisjointHistory {
			t.Errorf("a feature branch off main shares a common ancestor — must NOT be flagged disjoint, got %+v", truth)
		}
	})
}

// AbortRebase restores a worktree a session left mid-rebase to a clean state
// before the harness keeps it for a human. It is best-effort (no return) — a
// no-op `rebase --abort` when none is in progress fails harmlessly.
func TestAbortRebaseRunsAbort(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	abortRebase("/wt", run)
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one git call, got %d: %v", len(*calls), *calls)
	}
	got := strings.Join((*calls)[0], " ")
	if want := "git -C /wt rebase --abort"; got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestStripWorktreeNodeModulesRemovesIt(t *testing.T) {
	wt := t.TempDir()
	binding := filepath.Join(wt, "web", "node_modules", "@oxlint", "binding-linux-arm64-gnu")
	if err := os.MkdirAll(binding, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := StripWorktreeNodeModules(wt); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if _, err := os.Stat(filepath.Join(wt, "web", "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("web/node_modules should be gone, stat err = %v", err)
	}
}

// Idempotent: a worktree that never ran `pnpm install` (or was already stripped)
// must not be an error — the strip runs unconditionally on every handoff.
func TestStripWorktreeNodeModulesIsNoOpWhenAbsent(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "web"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := StripWorktreeNodeModules(wt); err != nil {
		t.Fatalf("expected no error when node_modules is absent, got %v", err)
	}
}

// The strip is surgical: only `web/node_modules` goes — the committed source the
// reviewer is here to read (including everything else under `web/`) stays put.
func TestStripWorktreeNodeModulesLeavesSourceIntact(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "web", "node_modules", "left-pad"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	src := filepath.Join(wt, "web", "src", "app.tsx")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(src, []byte("export const App = () => null"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := StripWorktreeNodeModules(wt); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("web/src/app.tsx should survive the strip, stat err = %v", err)
	}
}

// BEH-553: BranchExists is the retrospective's host-side precondition that the
// upstream /tdd step actually produced a feature branch. It reads the LOCAL head
// ref (where the worktree's commits land via the shared .git), so it must return
// false before feat/<slug> is created and true once it exists.
func TestBranchExistsTracksLocalFeatureBranch(t *testing.T) {
	repo := t.TempDir()
	gitInRepo := func(args ...string) {
		t.Helper()
		if err := execIn(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	gitInRepo("init", "-q")
	gitInRepo("config", "user.email", "t@example.com")
	gitInRepo("config", "user.name", "Test")
	gitInRepo("commit", "-q", "--allow-empty", "-m", "root")

	const slug = "beh-553-retro-precondition"
	if BranchExists(repo, slug) {
		t.Fatal("BranchExists should be false before feat/<slug> is created")
	}

	gitInRepo("branch", BranchName(slug))
	if !BranchExists(repo, slug) {
		t.Error("BranchExists should be true once feat/<slug> resolves")
	}

	// An unrelated slug must not resolve — the gate is keyed to the exact branch.
	if BranchExists(repo, "beh-999-nope") {
		t.Error("BranchExists must not report a branch that was never created")
	}

	// A path that isn't a git repo makes rev-parse fail; the documented contract
	// treats any git failure as "ref absent" (false), never a panic or true.
	if BranchExists(t.TempDir(), slug) {
		t.Error("BranchExists must read an unreadable ref (non-repo path) as absent")
	}
}

func TestPushUsesNoVerifyAndCorrectArgs(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(0, nil)
	if err := push("/herd", "beh-403", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one attempt, got %d", len(*calls))
	}
	got := (*calls)[0]
	want := []string{"git", "-C", "/herd", "push", "--no-verify", "origin", "feat/beh-403"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// BEH-570: after an auto-rebase rewrites the branch's history, the re-push to an
// already-pushed branch must force — but safely. --force-with-lease refuses to
// clobber remote commits the harness hasn't observed, and --no-verify skips the
// host pre-push hook (same rationale as Push). It is retried like every remote op.
func TestPushForceWithLeaseUsesLeaseAndNoVerify(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(0, nil)
	if err := pushForceWithLease("/herd", "beh-570", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one attempt, got %d", len(*calls))
	}
	got := (*calls)[0]
	want := []string{"git", "-C", "/herd", "push", "--no-verify", "--force-with-lease", "origin", "feat/beh-570"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestPushForceWithLeaseRetriesThenSucceeds(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(2, errors.New("exit status 128"))
	if err := pushForceWithLease("/herd", "beh-570", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("expected 3 attempts (2 fail + 1 ok), got %d", len(*calls))
	}
}

func TestPushRetriesThenSucceeds(t *testing.T) {
	// Two transient failures, then success — must not be stranded.
	clock := newFakeClock()
	run, calls := scriptedRunner(2, errors.New("exit status 128"))
	if err := push("/herd", "beh-403", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("expected 3 attempts (2 fail + 1 ok), got %d", len(*calls))
	}
}

// The core of BEH-403: a disruption lasting a couple of minutes (far beyond the
// old ~4s window) must not strand a green branch — the loop keeps retrying until
// the blip clears.
func TestPushRidesOutMultiMinuteBlip(t *testing.T) {
	clock := newFakeClock()
	run, calls := blipRunner(clock, 2*time.Minute, errors.New("exit status 128"))
	if err := push("/herd", "beh-403", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success once the blip cleared, got %v", err)
	}
	if *calls < 2 {
		t.Fatalf("expected multiple attempts across the blip, got %d", *calls)
	}
	if clock.elapsed() < 2*time.Minute {
		t.Fatalf("retry span %v did not ride out the 2-minute blip", clock.elapsed())
	}
}

// An indefinite outage must terminate — but only after spending close to the full
// budget, not after the old 4s window — and never exceed the budget.
func TestPushGivesUpNearBudgetAfterPersistentFailure(t *testing.T) {
	clock := newFakeClock()
	boom := errors.New("exit status 128")
	run, calls := scriptedRunner(1<<30, boom) // always fails
	err := push("/herd", "beh-403", run, clock.sleep, clock.now)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the last error %v, got %v", boom, err)
	}
	if clock.elapsed() > remoteRetryBudget {
		t.Fatalf("retry span %v exceeded the budget %v", clock.elapsed(), remoteRetryBudget)
	}
	if clock.elapsed() < remoteRetryBudget-remoteMaxDelay {
		t.Fatalf("retry span %v gave up well before the budget %v", clock.elapsed(), remoteRetryBudget)
	}
	if len(*calls) < 2 {
		t.Fatalf("expected many attempts within the budget, got %d", len(*calls))
	}
}

// BEH-475: a 401/403 is a permanent auth failure — the token is wrong, expired,
// or lacks the repo grant, so every retry fails identically. Retrying it burns the
// whole 5-min budget as dead time that, worse, looks like a hang. It must fail after
// a single attempt with no backoff sleeps, surfacing the underlying error.
func TestPushFailsFastOnPermanentAuthFailure(t *testing.T) {
	clock := newFakeClock()
	sleeps := 0
	sleep := func(d time.Duration) { sleeps++; clock.sleep(d) }
	authErr := errors.New("exit status 128: fatal: unable to access " +
		"'https://github.com/org/herd.git/': The requested URL returned error: 403")
	run, calls := scriptedRunner(1<<30, authErr) // would fail forever if retried
	err := push("/herd", "beh-475", run, sleep, clock.now)
	if !errors.Is(err, authErr) {
		t.Fatalf("expected the underlying auth error preserved, got %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one attempt (no retry on permanent auth failure), got %d", len(*calls))
	}
	if sleeps != 0 {
		t.Fatalf("expected no backoff sleeps on a permanent failure, got %d", sleeps)
	}
}

// The classifier boundary (BEH-475): only credential rejections short-circuit the
// retry budget. A transient class — DNS, 5xx, a timeout kill, or a bare exit-128
// with no auth phrase — must stay retryable, or an over-broad marker would silently
// turn every momentary blip into an instant give-up (the exact regression BEH-403
// widened the budget to prevent).
func TestNonTransientClassifiesOnlyAuthFailures(t *testing.T) {
	permanent := []string{
		"exit status 128: fatal: unable to access '…/herd.git/': The requested URL returned error: 403",
		"exit status 128: fatal: unable to access '…/herd.git/': The requested URL returned error: 401",
		"exit status 128: fatal: Authentication failed for 'https://github.com/org/herd.git/'",
		"exit status 128: fatal: could not read Username for 'https://github.com': terminal prompts disabled",
		"exit status 1: ERROR: Write access to repository not granted.",
		"exit status 22: error: RPC failed; HTTP 403 Forbidden",
	}
	transient := []string{
		"exit status 128",
		"exit status 128: fatal: unable to access '…': Could not resolve host: github.com",
		"exit status 128: error: RPC failed; HTTP 500 curl 22",
		"exit status 128: error: cannot lock ref 'refs/remotes/origin/main': is at … but expected …",
		`"git" did not respond within 2m0s: command timed out`,
	}
	for _, msg := range permanent {
		if !nonTransient(errors.New(msg)) {
			t.Errorf("expected non-transient (fail fast): %q", msg)
		}
	}
	for _, msg := range transient {
		if nonTransient(errors.New(msg)) {
			t.Errorf("expected transient (keep retrying): %q", msg)
		}
	}
	if nonTransient(nil) {
		t.Error("nil error must not be classified non-transient")
	}
}

func TestFetchMainRetriesThenSucceeds(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(1, errors.New("exit status 128"))
	if err := fetchMain("/herd", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 attempts (1 fail + 1 ok), got %d", len(*calls))
	}
	got := (*calls)[0]
	want := []string{"git", "-C", "/herd", "fetch", "-q", "origin", "main"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWithRetryBackoffIsExponentialAndCapped(t *testing.T) {
	clock := newFakeClock()
	var delays []time.Duration
	sleep := func(d time.Duration) { delays = append(delays, d); clock.sleep(d) }
	if err := withRetry(func() error { return errors.New("always") }, sleep, clock.now); err == nil {
		t.Fatal("expected an error when op always fails")
	}
	if len(delays) < 2 {
		t.Fatalf("expected several backoff sleeps, got %d", len(delays))
	}
	if delays[0] != remoteBaseDelay {
		t.Fatalf("first delay = %v, want base %v", delays[0], remoteBaseDelay)
	}
	if delays[1] != 2*remoteBaseDelay {
		t.Fatalf("second delay = %v, want %v (doubled)", delays[1], 2*remoteBaseDelay)
	}
	for i, d := range delays {
		if d > remoteMaxDelay {
			t.Fatalf("delay[%d] = %v exceeds cap %v", i, d, remoteMaxDelay)
		}
	}
	if last := delays[len(delays)-1]; last != remoteMaxDelay {
		t.Fatalf("last delay = %v, want it to have grown to the cap %v", last, remoteMaxDelay)
	}
}

func TestWithRetryNoSleepOnFirstSuccess(t *testing.T) {
	clock := newFakeClock()
	sleeps := 0
	sleep := func(d time.Duration) { sleeps++; clock.sleep(d) }
	if err := withRetry(func() error { return nil }, sleep, clock.now); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if sleeps != 0 {
		t.Fatalf("expected no sleeps on immediate success, got %d", sleeps)
	}
}
