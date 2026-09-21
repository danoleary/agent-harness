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

// BranchName derives the canonical feature branch from the Consumer's configured
// branch prefix + slug (BEH-636); the harness keys verify/push/PR/dispatch off it,
// so a Consumer that sets branch_prefix = "fix" gets `fix/<slug>`, not `feat/<slug>`.
func TestBranchNameDerivesFromPrefix(t *testing.T) {
	if got := Open("/herd", "feat").Worktree("beh-636-x").Branch(); got != "feat/beh-636-x" {
		t.Errorf(`Worktree("beh-636-x").Branch() under prefix "feat" = %q, want "feat/beh-636-x"`, got)
	}
	if got := Open("/herd", "fix").Worktree("beh-636-x").Branch(); got != "fix/beh-636-x" {
		t.Errorf(`Worktree("beh-636-x").Branch() under prefix "fix" = %q, want "fix/beh-636-x"`, got)
	}
}

// The worktree path is derived from the checkout the same way — `.claude/worktrees/<slug>`
// under the Consumer's checkout — so a caller names the ticket and nothing else.
func TestWorktreePathDerivesFromCheckout(t *testing.T) {
	got := Open("/herd", "feat").Worktree("beh-636-x").Path()
	if want := filepath.Join("/herd", ".claude", "worktrees", "beh-636-x"); got != want {
		t.Errorf("Worktree path = %q, want %q", got, want)
	}
}

// BEH-636: the harness now creates the worktree + canonical branch host-side
// (retiring the sandbox-runs-new-worktree.sh coupling). CreateWorktree adds the
// worktree on `<branchPrefix>/<slug>` based on the freshly-fetched origin/main.
func TestCreateWorktreeAddsBranchFromPrefixOntoOriginMain(t *testing.T) {
	// Fresh run: the feature branch does not yet exist (refs/heads probe fails), so
	// the `-b`-off-origin/main path runs, not the resumed-branch attach.
	run, calls := recordingRunner(func(args []string) error {
		if strings.Contains(strings.Join(args, " "), "refs/heads/") {
			return errors.New("no such branch yet")
		}
		return nil
	})
	wt := fakeWorktree("beh-636-x", runners{run: run})
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var add []string
	for _, c := range *calls {
		if hasArg(c, "worktree") && hasArg(c, "add") {
			add = c
		}
	}
	if add == nil {
		t.Fatalf("expected a `git worktree add`, calls: %v", *calls)
	}
	joined := strings.Join(add, " ")
	if !strings.Contains(joined, "-b feat/beh-636-x") {
		t.Errorf("worktree add must create the prefix-derived branch, got %q", joined)
	}
	if !strings.Contains(joined, wt.Path()) {
		t.Errorf("worktree add must target the worktree path %q, got %q", wt.Path(), joined)
	}
	if !strings.Contains(joined, "refs/remotes/origin/main") {
		t.Errorf("worktree add must base on origin/main when it resolves, got %q", joined)
	}
}

// When origin/main can't be resolved (no remote-tracking ref — a fresh clone or an
// offline host), CreateWorktree falls back to local `main` rather than failing, so
// a first run on a repo without a fetched origin/main still gets a worktree.
func TestCreateWorktreeFallsBackToLocalMain(t *testing.T) {
	run, calls := recordingRunner(func(args []string) error {
		if strings.Contains(strings.Join(args, " "), "rev-parse --verify") {
			return errors.New("no origin/main")
		}
		return nil
	})
	if err := fakeWorktree("beh-636-x", runners{run: run}).Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var add []string
	for _, c := range *calls {
		if hasArg(c, "worktree") && hasArg(c, "add") {
			add = c
		}
	}
	joined := strings.Join(add, " ")
	if strings.Contains(joined, "origin/main") {
		t.Errorf("with origin/main unresolved, add must base on local main, got %q", joined)
	}
	if !hasArg(add, "main") {
		t.Errorf("expected local `main` as the base, got %q", joined)
	}
}

// Resumed-branch case (BEH-554): a prior session already created `<branch>` with
// un-merged commits but its worktree dir was torn down. CreateWorktree must
// RE-ATTACH a worktree to the existing branch (`worktree add <path> <branch>`, no
// `-b`) rather than `-b` it (which fails "branch already exists") — preserving the
// prior work for the resumed-branch session to verify-and-handoff.
func TestCreateWorktreeAttachesToExistingBranch(t *testing.T) {
	run, calls := recordingRunner(func(args []string) error {
		// The feature branch already resolves; origin/main does not matter here.
		if strings.Contains(strings.Join(args, " "), "rev-parse --verify -q refs/heads/feat/beh-636-x") {
			return nil
		}
		if strings.Contains(strings.Join(args, " "), "rev-parse") {
			return errors.New("only the feature branch resolves")
		}
		return nil
	})
	if err := fakeWorktree("beh-636-x", runners{run: run}).Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var add []string
	for _, c := range *calls {
		if hasArg(c, "worktree") && hasArg(c, "add") {
			add = c
		}
	}
	joined := strings.Join(add, " ")
	if hasArg(add, "-b") {
		t.Errorf("attaching to an existing branch must NOT use `-b` (that fails 'branch already exists'), got %q", joined)
	}
	if !hasArg(add, "feat/beh-636-x") {
		t.Errorf("attach must target the existing branch, got %q", joined)
	}
}

// A failing `worktree add` (e.g. the branch already exists) is surfaced, not
// swallowed — the caller must know the worktree wasn't created.
func TestCreateWorktreePropagatesAddError(t *testing.T) {
	run, _ := recordingRunner(func(args []string) error {
		if strings.Contains(strings.Join(args, " "), "worktree add") {
			return errors.New("fatal: branch already exists")
		}
		return nil
	})
	if err := fakeWorktree("beh-636-x", runners{run: run}).Create(); err == nil {
		t.Fatal("expected Create to propagate a failing `worktree add`")
	}
}

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

// recordingRunner returns a commandRunner that records every argv it saw and
// returns whatever fail(args) decides — letting a test branch the control flow on
// the specific git subcommand (not just call order, which scriptedRunner keys on).
func recordingRunner(fail func(args []string) error) (commandRunner, *[][]string) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return fail(args)
	}
	return run, &calls
}

// hasArg reports whether any element of an argv exactly equals s — a precise git
// subcommand check that won't false-match a ref like refs/harness/rebase-onto-main.
func hasArg(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}

// testRepo/testPrefix are the checkout and branch prefix every seam test binds a
// Checkout to. They are the only two strings a test has to supply: the branch and
// the worktree path follow from the slug, exactly as they do in production.
const (
	testRepo   = "/herd"
	testPrefix = "feat"
)

// fakeCheckout builds a Checkout over an injected seam — the one hook this package
// has. Any field left zero gets a harmless no-op, so a test overrides only the
// primitive it is about; everything else behaves as a silent success.
//
// It is what lets a test drive the EXPORTED method production calls. The twelve
// exported/unexported twin pairs this replaces meant the tested half was never the
// half the harness ran: Push, PushForceWithLease and Create — the operations that
// can strand a branch — sat entirely on the untested side, and with them the whole
// transient-retry budget.
func fakeCheckout(r runners) Checkout {
	if r.run == nil {
		r.run = func(string, ...string) error { return nil }
	}
	if r.output == nil {
		r.output = func(string, ...string) ([]byte, error) { return nil, nil }
	}
	if r.pipe == nil {
		r.pipe = func([]byte, string, ...string) ([]byte, error) { return nil, nil }
	}
	if r.sleep == nil {
		r.sleep = func(time.Duration) {}
	}
	if r.now == nil {
		r.now = func() time.Time { return time.Unix(0, 0) }
	}
	return Checkout{path: testRepo, branchPrefix: testPrefix, runners: r}
}

// fakeWorktree is fakeCheckout's per-ticket shorthand.
func fakeWorktree(slug string, r runners) Worktree { return fakeCheckout(r).Worktree(slug) }

// branchName is the test-side spelling of the branch a Worktree derives, for the
// setup `git checkout -b` calls that must create exactly that ref.
func branchName(slug string) string { return testPrefix + "/" + slug }

// realWorktree binds a Worktree to a throwaway repo a test just built, over the
// PRODUCTION seam — these tests run real git and pin the contracts the scripted
// ones rest on. repo and path are separate because a linked worktree (the shape
// production always uses) is checked out somewhere other than the checkout it
// hangs off; the flat-repo tests pass the same directory twice.
func realWorktree(repo, path, slug string) Worktree {
	return Worktree{repo: repo, branch: branchName(slug), path: path, runners: execRunners()}
}

// BEH-618: a long pipeline lets sibling PRs merge to main underneath the branch,
// stranding it on a stale base. RebaseOntoMain replays the feature branch onto the
// freshly-fetched origin/main before the push so the PR opens on a current base. It
// must NOT use `git rebase`: in a linked worktree (every harness worktree is a `git
// worktree add` checkout) rebase's detach-to-onto checkout false-fails with "local
// changes would be overwritten by merge" / "could not detach HEAD" even on a
// byte-clean tree. The replay instead goes via `reset --hard origin/main` (the same
// full-tree move, without the false-positive) + `cherry-pick` (a three-way merge
// that surfaces only genuine conflicts). A clean replay reports RebaseClean, never
// invokes `git rebase`, and carries the harness identity on the replayed commits.
// The replay uses a bare `cherry-pick` (no `--empty=drop`, which needs git 2.45+ and
// is rejected on the git 2.39 CI runners) — the empty-commit cases are handled by the
// auto-skip loop instead.
func TestRebaseOntoMainReplaysViaResetAndCherryPickNotRebase(t *testing.T) {
	run, calls := recordingRunner(func(args []string) error {
		// The feature tip is NOT yet contained in origin/main, so the ancestor probe
		// reports "not behind" and the cherry-pick replay path runs.
		if strings.Contains(strings.Join(args, " "), "merge-base --is-ancestor") {
			return errors.New("not an ancestor")
		}
		return nil
	})
	if res := fakeWorktree("beh-618", runners{run: run}).Rebase(); res != RebaseClean {
		t.Fatalf("res = %v, want RebaseClean", res)
	}

	var sawReset, sawCherryPick bool
	for _, c := range *calls {
		if hasArg(c, "rebase") {
			t.Fatalf("rebaseOntoMain must NOT invoke `git rebase` in a linked worktree (BEH-618); saw %q", strings.Join(c, " "))
		}
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "reset --hard origin/main") {
			sawReset = true
		}
		if strings.Contains(joined, "cherry-pick origin/main..") {
			sawCherryPick = true
			// The replay must NOT carry `--empty=drop` (unsupported before git 2.45).
			if strings.Contains(joined, "--empty=drop") {
				t.Errorf("cherry-pick replay must be bare (no --empty=drop, unsupported on git < 2.45), got %q", joined)
			}
			// The replay carries the harness identity overrides (BEH-579) so replayed
			// commits' committer never inherits the host's placeholder config.
			if !strings.Contains(joined, "user.name="+HarnessAuthorName) || !strings.Contains(joined, "user.email="+HarnessAuthorEmail) {
				t.Errorf("cherry-pick replay must carry the harness identity overrides, got %q", joined)
			}
		}
	}
	if !sawReset {
		t.Errorf("expected a `reset --hard origin/main` to move the branch onto the fresh base, calls: %v", *calls)
	}
	if !sawCherryPick {
		t.Errorf("expected a `cherry-pick origin/main..<tip>` to replay the feature commits, calls: %v", *calls)
	}
}

// A replay that can't be applied cleanly (a genuine content conflict surfaces during
// cherry-pick) must abort the cherry-pick AND restore the branch to its original tip
// — never left mid-replay — then report RebaseConflict so the caller defers to a
// human rather than pushing.
func TestRebaseOntoMainCherryPickConflictAbortsAndRestores(t *testing.T) {
	run, calls := recordingRunner(func(args []string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "merge-base --is-ancestor") {
			return errors.New("not an ancestor")
		}
		if strings.Contains(joined, "cherry-pick origin/main..") {
			return errors.New("exit status 1: CONFLICT (content)")
		}
		// A genuine content conflict halts mid-cherry-pick (CHERRY_PICK_HEAD resolves,
		// so the default nil above reports "in progress") and — unlike an initially-empty
		// halt — leaves UNMERGED index entries. That unmerged signal is what makes the
		// harness abort rather than auto-skip (BEH-678).
		if strings.Contains(joined, "diff --quiet --diff-filter=U") {
			return errors.New("exit status 1: unmerged paths present")
		}
		return nil
	})
	if res := fakeWorktree("beh-618", runners{run: run}).Rebase(); res != RebaseConflict {
		t.Fatalf("res = %v, want RebaseConflict", res)
	}
	var sawCherryAbort, sawRestore, sawSkip bool
	for _, c := range *calls {
		if hasArg(c, "rebase") {
			t.Fatalf("the conflict path must NOT invoke `git rebase` (BEH-618); saw %q", strings.Join(c, " "))
		}
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "cherry-pick --abort") {
			sawCherryAbort = true
		}
		if strings.Contains(joined, "reset --hard "+rebaseBackupRef) {
			sawRestore = true
		}
		if strings.Contains(joined, "cherry-pick --skip") {
			sawSkip = true
		}
	}
	if sawSkip {
		t.Errorf("a genuine content conflict (unmerged paths) must NOT be auto-skipped as an empty commit (BEH-678), calls: %v", *calls)
	}
	if !sawCherryAbort {
		t.Errorf("a conflicting replay must `cherry-pick --abort`, calls: %v", *calls)
	}
	if !sawRestore {
		t.Errorf("a conflicting replay must restore the branch to its original tip via the backup ref %q, calls: %v", rebaseBackupRef, *calls)
	}
}

// BEH-678: a bare `cherry-pick` HALTS on any empty commit with exit 1 ("the previous
// cherry-pick is now empty") — both a commit that BECOMES empty on replay (BEH-622)
// and a commit that was INITIALLY empty (an `--allow-empty` handoff commit with a zero
// net diff — the d0177234b case). Neither is a content conflict — escalating one to a
// sandboxed conflict-resolution session (BEH-581) burns a whole session on an empty
// commit. rebaseOntoMain must recognise the halt has NO unmerged paths (clean index)
// and `cherry-pick --skip` it inline, then report RebaseClean, matching an ideal `git
// rebase`'s drop of an empty commit.
func TestRebaseOntoMainSkipsInitiallyEmptyCommitInline(t *testing.T) {
	skipped := false
	run, calls := recordingRunner(func(args []string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "merge-base --is-ancestor") {
			return errors.New("not an ancestor")
		}
		// The replay halts on the initially-empty commit.
		if strings.Contains(joined, "cherry-pick origin/main..") {
			return errors.New("exit status 1: the previous cherry-pick is now empty")
		}
		// A cherry-pick is in progress until the empty commit is skipped; afterwards the
		// sequence is complete and CHERRY_PICK_HEAD no longer resolves.
		if strings.Contains(joined, "CHERRY_PICK_HEAD") {
			if skipped {
				return errors.New("no CHERRY_PICK_HEAD")
			}
			return nil
		}
		// The halted commit is empty, so the index carries no unmerged paths.
		if strings.Contains(joined, "diff --quiet --diff-filter=U") {
			return nil
		}
		if strings.Contains(joined, "cherry-pick --skip") {
			skipped = true
			return nil
		}
		return nil
	})
	if res := fakeWorktree("beh-618", runners{run: run}).Rebase(); res != RebaseClean {
		t.Fatalf("res = %v, want RebaseClean — an initially-empty commit must be auto-skipped inline, not escalated as a conflict (BEH-678)", res)
	}
	var sawSkip, sawAbort bool
	for _, c := range *calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "cherry-pick --skip") {
			sawSkip = true
		}
		if strings.Contains(joined, "cherry-pick --abort") {
			sawAbort = true
		}
	}
	if !sawSkip {
		t.Errorf("an initially-empty commit must be dropped via `cherry-pick --skip`, calls: %v", *calls)
	}
	if sawAbort {
		t.Errorf("an initially-empty commit is not a conflict — must NOT `cherry-pick --abort`, calls: %v", *calls)
	}
}

// BEH-678: the auto-skip path must stay fail-safe. If the replay cherry-pick errors
// but leaves NO cherry-pick in progress (an unexpected git failure, not a halt on an
// empty commit or a conflict), the harness must NOT fall through to RebaseClean and
// push an incompletely-replayed branch — it aborts and reports RebaseConflict, exactly
// as a genuine conflict would, so a push never happens on doubt.
func TestRebaseOntoMainCherryPickErrorWithoutPickInProgressIsConflict(t *testing.T) {
	run, calls := recordingRunner(func(args []string) error {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "merge-base --is-ancestor") {
			return errors.New("not an ancestor")
		}
		if strings.Contains(joined, "cherry-pick origin/main..") {
			return errors.New("exit status 128: bad revision")
		}
		// No cherry-pick is in progress after the failure — CHERRY_PICK_HEAD never resolves.
		if strings.Contains(joined, "CHERRY_PICK_HEAD") {
			return errors.New("no CHERRY_PICK_HEAD")
		}
		return nil
	})
	if res := fakeWorktree("beh-618", runners{run: run}).Rebase(); res != RebaseConflict {
		t.Fatalf("res = %v, want RebaseConflict — an unexpected cherry-pick failure must fail safe, never RebaseClean (BEH-678)", res)
	}
	var sawSkip, sawRestore bool
	for _, c := range *calls {
		joined := strings.Join(c, " ")
		if strings.Contains(joined, "cherry-pick --skip") {
			sawSkip = true
		}
		if strings.Contains(joined, "reset --hard "+rebaseBackupRef) {
			sawRestore = true
		}
	}
	if sawSkip {
		t.Errorf("nothing to skip when no cherry-pick is in progress, calls: %v", *calls)
	}
	if !sawRestore {
		t.Errorf("an unexpected failure must restore the branch to its original tip, calls: %v", *calls)
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
		git("checkout", "-q", "-b", branchName(slug))
		write("feat.txt", "feat change\n")
		git("add", "-A")
		git("commit", "-q", "-m", "feat work")
		// main advances by one commit; origin/main tracks it.
		git("checkout", "-q", "main")
		write(mainFile, mainContents)
		git("add", "-A")
		git("commit", "-q", "-m", "main moved")
		git("update-ref", "refs/remotes/origin/main", "main")
		git("checkout", "-q", branchName(slug))
		return repo
	}

	t.Run("clean replay onto an advanced base", func(t *testing.T) {
		// main touched a different file, so feat replays cleanly onto the new base.
		repo := setupRepo(t, "unrelated.txt", "main-only\n")
		if res := realWorktree(repo, repo, slug).Rebase(); res != RebaseClean {
			t.Fatalf("res = %v, want RebaseClean", res)
		}
		// The replayed branch now sits on top of main: it carries main's new file.
		if _, err := os.Stat(filepath.Join(repo, "unrelated.txt")); err != nil {
			t.Fatalf("rebased branch should contain main's commit (unrelated.txt), stat err = %v", err)
		}
		if !realWorktree(repo, repo, slug).Clean() {
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
		featTip, err := realWorktree(repo, repo, slug).HeadSHA()
		if err != nil {
			t.Fatalf("read feat tip: %v", err)
		}
		if res := realWorktree(repo, repo, slug).Rebase(); res != RebaseConflict {
			t.Fatalf("res = %v, want RebaseConflict", res)
		}
		// Aborted cleanly: no rebase in progress, branch back at its original tip.
		if !realWorktree(repo, repo, slug).Clean() {
			t.Fatal("worktree should be clean after the conflicting rebase was aborted")
		}
		after, err := realWorktree(repo, repo, slug).HeadSHA()
		if err != nil {
			t.Fatalf("read tip after abort: %v", err)
		}
		if after != featTip {
			t.Fatalf("branch tip = %s after abort, want it restored to %s", after, featTip)
		}
	})
}

// BEH-622: when a feature commit's diff is already present identically in
// origin/main (a sibling PR merged the same change, or a hotfix was cherry-picked
// to main), replaying it makes the cherry-pick "now empty" and a bare `git
// cherry-pick` HALTS with exit 1 ("use 'git cherry-pick --skip'"). rebaseOntoMain
// would misread that as a genuine content conflict and burn a sandboxed
// resolution session on a replay an ideal `git rebase` would auto-drop clean.
// rebaseOntoMain's auto-skip loop recognises the empty halt (clean index, no unmerged
// paths) and `cherry-pick --skip`s the redundant commit, keeping the non-redundant
// ones — so the replay reports RebaseClean. (This is why the replay uses a bare
// cherry-pick, not `--empty=drop`, which is unsupported on the git 2.39 CI runners.)
// The setup gives feat TWO commits (one redundant with main, one not) so the test
// proves the redundant one is dropped AND the other is preserved.
func TestRebaseOntoMainDropsRedundantCommit(t *testing.T) {
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
	const slug = "beh-622-real"
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("feat.txt", "base\n")
	write("shared.txt", "orig\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	// feat branch makes two commits: the first sets shared.txt to a value main will
	// also adopt (becomes redundant on replay); the second is feat-only (must survive).
	git("checkout", "-q", "-b", branchName(slug))
	write("shared.txt", "shared change\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat redundant")
	write("feat.txt", "feat change\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat keep")
	// main advances: it adopts the SAME shared.txt change (making feat's first commit
	// redundant) plus an unrelated file. origin/main tracks that tip.
	git("checkout", "-q", "main")
	write("shared.txt", "shared change\n")
	write("unrelated.txt", "main-only\n")
	git("add", "-A")
	git("commit", "-q", "-m", "main moved")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", branchName(slug))

	if res := realWorktree(repo, repo, slug).Rebase(); res != RebaseClean {
		t.Fatalf("res = %v, want RebaseClean — a commit already present in origin/main must be auto-dropped, not misread as a conflict (BEH-622)", res)
	}
	if !realWorktree(repo, repo, slug).Clean() {
		t.Fatal("worktree should be clean after the redundant commit was dropped")
	}
	// The non-redundant commit survived: feat.txt carries feat's change.
	feat, err := os.ReadFile(filepath.Join(repo, "feat.txt"))
	if err != nil || string(feat) != "feat change\n" {
		t.Fatalf("feat.txt = %q (err %v), want %q — the non-redundant commit must be preserved", string(feat), err, "feat change\n")
	}
	// The branch sits on top of main: it carries main's unrelated file.
	if _, err := os.Stat(filepath.Join(repo, "unrelated.txt")); err != nil {
		t.Fatalf("rebased branch should contain main's commit (unrelated.txt), stat err = %v", err)
	}
	// Exactly ONE commit replayed on top of origin/main — the redundant one was
	// dropped (not kept as an empty commit), matching an ideal rebase's auto-drop.
	out, err := exec.Command("git", "-C", repo, "rev-list", "--count", "origin/main..HEAD").Output()
	if err != nil {
		t.Fatalf("count replayed commits: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("replayed commit count = %s, want 1 (the redundant commit must be dropped, the other kept)", got)
	}
}

// BEH-678: the real-git companion to the scripted TestRebaseOntoMainSkipsInitially-
// EmptyCommitInline. A bare cherry-pick HALTS on a commit that was INITIALLY empty (an
// `--allow-empty` handoff commit with a zero net diff — the d0177234b case) with "the
// previous cherry-pick is now empty", just as it does on a becomes-empty commit
// (BEH-622). rebaseOntoMain must auto-skip it inline and land a clean replay, not
// misread it as a content conflict and burn a sandboxed session.
// feat gets a real commit AND an --allow-empty commit so the test proves the empty one
// is dropped while the real one is preserved — pinned against real git, not a fake, so
// a wrong flag or a git-semantics surprise can't slip through.
func TestRebaseOntoMainSkipsInitiallyEmptyCommitAgainstRealGit(t *testing.T) {
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
	const slug = "beh-678-real"
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("feat.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	// feat branch makes a real commit, then an INITIALLY-empty --allow-empty commit
	// (zero net diff — the shape a superseded ticket's documentation handoff leaves).
	git("checkout", "-q", "-b", branchName(slug))
	write("feat.txt", "feat change\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat keep")
	git("commit", "-q", "--allow-empty", "-m", "docs: superseded handoff (empty)")
	// main advances by an unrelated file; origin/main tracks that tip.
	git("checkout", "-q", "main")
	write("unrelated.txt", "main-only\n")
	git("add", "-A")
	git("commit", "-q", "-m", "main moved")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", branchName(slug))

	if res := realWorktree(repo, repo, slug).Rebase(); res != RebaseClean {
		t.Fatalf("res = %v, want RebaseClean — an initially-empty commit must be auto-skipped inline, not misread as a conflict (BEH-678)", res)
	}
	if !realWorktree(repo, repo, slug).Clean() {
		t.Fatal("worktree should be clean after the initially-empty commit was skipped")
	}
	// The real commit survived: feat.txt carries feat's change.
	feat, err := os.ReadFile(filepath.Join(repo, "feat.txt"))
	if err != nil || string(feat) != "feat change\n" {
		t.Fatalf("feat.txt = %q (err %v), want %q — the real commit must be preserved", string(feat), err, "feat change\n")
	}
	// The branch sits on top of main: it carries main's unrelated file.
	if _, err := os.Stat(filepath.Join(repo, "unrelated.txt")); err != nil {
		t.Fatalf("rebased branch should contain main's commit (unrelated.txt), stat err = %v", err)
	}
	// Exactly ONE commit replayed on top of origin/main — the initially-empty one was
	// dropped (not kept), matching an ideal rebase's drop of an empty commit.
	out, err := exec.Command("git", "-C", repo, "rev-list", "--count", "origin/main..HEAD").Output()
	if err != nil {
		t.Fatalf("count replayed commits: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("replayed commit count = %s, want 1 (the initially-empty commit must be dropped, the real one kept)", got)
	}
}

// BEH-617: new-worktree.sh drops an untracked .worktree-ready sentinel (BEH-549)
// into every worktree, and `git rebase`'s checkout phase refuses to overwrite an
// untracked file regardless of .gitignore — aborting with "untracked working tree
// files would be overwritten by checkout". Left in place that makes a sentinel-only
// collision abort the rebase, which RebaseOntoMain would misreport as a content
// RebaseConflict and burn a sandboxed resolution session on. rebaseOntoMain must
// strip the sentinel first so the replay is never blocked by it. The setup makes the
// onto commit (origin/main) track .worktree-ready while the worktree holds an
// untracked copy — the exact shape that triggers the checkout refusal.
func TestRebaseOntoMainStripsReadySentinelBeforeRebase(t *testing.T) {
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
	const slug = "beh-617-real"
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("feat.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", branchName(slug))
	write("feat.txt", "feat change\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat work")
	// main advances on an unrelated file AND commits the sentinel as a tracked path,
	// so checking it out wants to write .worktree-ready over the worktree's untracked
	// copy — the "untracked would be overwritten by checkout" abort.
	git("checkout", "-q", "main")
	write("unrelated.txt", "main-only\n")
	write(WorktreeReadySentinel, "")
	git("add", "-A")
	git("commit", "-q", "-m", "main moved")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", branchName(slug))
	// The live worktree carries the untracked readiness sentinel new-worktree.sh drops.
	write(WorktreeReadySentinel, "")

	if res := realWorktree(repo, repo, slug).Rebase(); res != RebaseClean {
		t.Fatalf("res = %v, want RebaseClean — the untracked sentinel must not block (or be misreported as a conflict for) the rebase", res)
	}
	// The replayed branch sits on top of main: it carries main's new file.
	if _, err := os.Stat(filepath.Join(repo, "unrelated.txt")); err != nil {
		t.Fatalf("rebased branch should contain main's commit (unrelated.txt), stat err = %v", err)
	}
}

// BEH-618: every harness worktree is a `git worktree add`-LINKED checkout, and that
// is exactly where `git rebase`'s detach-to-onto checkout false-fails ("local changes
// would be overwritten by merge" / "could not detach HEAD") even on a byte-clean tree.
// The scripted tests above pin the reset+cherry-pick mechanism; this one pins the
// outcome in the real environment that triggered the bug — feat replays onto an
// advanced base inside a linked worktree (clean and conflicting), where the old bare
// `git rebase` was unreliable. setupLinked builds a main repo whose feat/<slug> lives
// in a separate linked worktree, with origin/main advanced past it.
func TestRebaseOntoMainReplaysInLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	const slug = "beh-618-linked"
	// setupLinked returns the linked worktree's path (for the test's own file reads)
	// and the [Worktree] the harness would hold for it — a worktree checked out
	// somewhere other than the checkout it hangs off, which is the production shape.
	// mainFile/mainContents control whether main's advancing commit collides with
	// feat.txt (conflict) or not (clean).
	setupLinked := func(t *testing.T, mainFile, mainContents string) (string, Worktree) {
		t.Helper()
		base := t.TempDir()
		git := func(dir string, args ...string) {
			t.Helper()
			if err := execIn(dir, args...); err != nil {
				t.Fatalf("git -C %s %v: %v", dir, args, err)
			}
		}
		write := func(dir, name, contents string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
		}
		git(base, "init", "-q", "-b", "main")
		git(base, "config", "user.email", "t@example.com")
		git(base, "config", "user.name", "Test")
		write(base, "feat.txt", "base\n")
		git(base, "add", "-A")
		git(base, "commit", "-q", "-m", "base")
		// feat branch edits feat.txt, then main advances; origin/main tracks main's tip.
		git(base, "checkout", "-q", "-b", branchName(slug))
		write(base, "feat.txt", "feat change\n")
		git(base, "add", "-A")
		git(base, "commit", "-q", "-m", "feat work")
		git(base, "checkout", "-q", "main")
		write(base, mainFile, mainContents)
		git(base, "add", "-A")
		git(base, "commit", "-q", "-m", "main moved")
		git(base, "update-ref", "refs/remotes/origin/main", "main")
		// Move feat into a LINKED worktree (and off the base checkout, so `worktree add`
		// can claim the branch). This is the harness's real shape (ADR-0002).
		git(base, "checkout", "-q", "main")
		linked := filepath.Join(t.TempDir(), "wt")
		git(base, "worktree", "add", "-q", linked, branchName(slug))
		// new-worktree.sh drops the untracked readiness sentinel in every worktree.
		write(linked, WorktreeReadySentinel, "")
		return linked, realWorktree(base, linked, slug)
	}

	t.Run("clean replay inside a linked worktree", func(t *testing.T) {
		linked, w := setupLinked(t, "unrelated.txt", "main-only\n")
		if res := w.Rebase(); res != RebaseClean {
			t.Fatalf("res = %v, want RebaseClean — a clean replay must not false-fail in a linked worktree (BEH-618)", res)
		}
		if _, err := os.Stat(filepath.Join(linked, "unrelated.txt")); err != nil {
			t.Fatalf("replayed branch should carry main's new file (unrelated.txt), stat err = %v", err)
		}
		if !w.Clean() {
			t.Fatal("linked worktree should be clean after a successful replay")
		}
		out, err := exec.Command("git", "-C", linked, "log", "-1", "--format=%cn|%ce").Output()
		if err != nil {
			t.Fatalf("read committer: %v", err)
		}
		if got, want := strings.TrimSpace(string(out)), HarnessAuthorName+"|"+HarnessAuthorEmail; got != want {
			t.Fatalf("replayed committer = %q, want %q (the placeholder Test identity must not leak)", got, want)
		}
	})

	t.Run("genuine conflict aborts and restores inside a linked worktree", func(t *testing.T) {
		_, w := setupLinked(t, "feat.txt", "main change\n")
		featTip, err := w.HeadSHA()
		if err != nil {
			t.Fatalf("read feat tip: %v", err)
		}
		if res := w.Rebase(); res != RebaseConflict {
			t.Fatalf("res = %v, want RebaseConflict", res)
		}
		if !w.Clean() {
			t.Fatal("linked worktree should be clean after the conflicting replay was aborted")
		}
		after, err := w.HeadSHA()
		if err != nil {
			t.Fatalf("read tip after abort: %v", err)
		}
		if after != featTip {
			t.Fatalf("branch tip = %s after abort, want it restored to %s", after, featTip)
		}
	})
}

// BEH-612: new-worktree.sh drops a readiness sentinel (.worktree-ready, BEH-549)
// into every worktree. It is gitignored on current main, but a feature branch based
// on a main that predates that .gitignore entry checks out a tree where the sentinel
// is NOT ignored — so `git status --porcelain` lists it as an untracked `??`, and the
// harness used to count it as uncommitted review work (blocking the push and
// committing the sentinel into the branch). WorktreeClean must treat the sentinel as
// a non-change regardless of .gitignore. The test repo has no .gitignore, so the
// sentinel shows as untracked — exactly the stale-base condition.
func TestWorktreeCleanIgnoresReadySentinel(t *testing.T) {
	wt, w := newRepoWithWorktree(t, "beh-612")
	if err := os.WriteFile(filepath.Join(wt, WorktreeReadySentinel), nil, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !w.Clean() {
		t.Fatal("a worktree whose only change is the readiness sentinel must be clean")
	}
}

// The sentinel filter must be surgical: real uncommitted work alongside the sentinel
// must still read as dirty, so the gate never waves through a tree that differs from
// what would ship.
func TestWorktreeCleanStillDetectsRealChangesAlongsideSentinel(t *testing.T) {
	wt, w := newRepoWithWorktree(t, "beh-612b")
	if err := os.WriteFile(filepath.Join(wt, WorktreeReadySentinel), nil, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "real.ts"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if w.Clean() {
		t.Fatal("a worktree with real uncommitted work must NOT be clean, even alongside the sentinel")
	}
}

// BEH-581: after a sandboxed conflict-resolution session, the harness must
// confirm the branch was ACTUALLY rebased onto origin/main before re-gating +
// pushing — a session that gave up and ran `git rebase --abort` leaves a clean
// worktree on the original stale tip, which IsRebasedOnto must catch. It is true
// iff the ref is an ancestor of HEAD (the branch contains the latest base).
func TestIsRebasedOntoTrueWhenAncestor(t *testing.T) {
	// `merge-base --is-ancestor` exits 0 → the ref is an ancestor → rebased.
	run, calls := scriptedRunner(0, nil)
	wt := fakeWorktree("beh-581", runners{run: run})
	if !wt.IsRebasedOnto("origin/main") {
		t.Fatal("exit 0 from merge-base --is-ancestor should report rebased (true)")
	}
	got := strings.Join((*calls)[0], " ")
	want := "git -C " + wt.Path() + " merge-base --is-ancestor origin/main HEAD"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestIsRebasedOntoFalseWhenNotAncestor(t *testing.T) {
	// A non-zero exit (ref not an ancestor) → NOT rebased.
	run, _ := scriptedRunner(1, errors.New("exit status 1"))
	if fakeWorktree("beh-581", runners{run: run}).IsRebasedOnto("origin/main") {
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
	git("checkout", "-q", "-b", branchName(slug))
	write("feat.txt", "feat\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat work")
	// main advances on an unrelated file; origin/main tracks it.
	git("checkout", "-q", "main")
	write("unrelated.txt", "main\n")
	git("add", "-A")
	git("commit", "-q", "-m", "main moved")
	git("update-ref", "refs/remotes/origin/main", "main")
	git("checkout", "-q", branchName(slug))

	// Before rebasing, origin/main is NOT an ancestor of feat's tip.
	if realWorktree(repo, repo, slug).IsRebasedOnto("origin/main") {
		t.Fatal("feat on its stale base should NOT read as rebased onto origin/main")
	}
	if res := realWorktree(repo, repo, slug).Rebase(); res != RebaseClean {
		t.Fatalf("setup rebase = %v, want RebaseClean", res)
	}
	// After a clean replay, origin/main IS an ancestor.
	if !realWorktree(repo, repo, slug).IsRebasedOnto("origin/main") {
		t.Fatal("after rebasing, feat should read as rebased onto origin/main")
	}
}

// BEH-597: IsDisjointFrom reports whether HEAD shares NO common ancestor with the
// ref — the disjoint-history condition (an empty `git merge-base`) the pre-push
// rebase must catch before mislabelling the inevitable collision a content
// conflict. `git merge-base <ref> HEAD` exits 0 when a common ancestor exists.
func TestIsDisjointFromFalseWhenCommonAncestor(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	wt := fakeWorktree("beh-597", runners{run: run})
	if wt.IsDisjointFrom("origin/main") {
		t.Fatal("exit 0 from merge-base (a common ancestor exists) should report NOT disjoint")
	}
	got := strings.Join((*calls)[0], " ")
	want := "git -C " + wt.Path() + " merge-base origin/main HEAD"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestIsDisjointFromTrueWhenNoCommonAncestor(t *testing.T) {
	// A non-zero exit (no merge base — disjoint histories) → disjoint.
	run, _ := scriptedRunner(1, errors.New("exit status 1"))
	if !fakeWorktree("beh-597", runners{run: run}).IsDisjointFrom("origin/main") {
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
	git("checkout", "-q", "-b", branchName("beh-597-joined"))
	write("feat.txt", "feat\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat work")
	if realWorktree(repo, repo, "beh-597-joined").IsDisjointFrom("origin/main") {
		t.Fatal("a feature branch off main shares a common ancestor — must NOT read as disjoint")
	}

	// An orphan branch (built with no parent) shares NO history with main → disjoint.
	git("checkout", "-q", "--orphan", branchName("beh-597-orphan"))
	write("orphan.txt", "orphan\n")
	git("add", "-A")
	git("commit", "-q", "-m", "orphan root")
	if !realWorktree(repo, repo, "beh-597-orphan").IsDisjointFrom("origin/main") {
		t.Fatal("an orphan branch with no common ancestor must read as disjoint")
	}
}

// BEH-609: RegraftOntoBase rescues verified work trapped on a disjoint branch by
// re-applying its CONTENT diff against ref onto a fresh commit rooted at ref —
// escaping the disjoint history so the branch can finally rebase and hand off. The
// regrafted branch must share history with ref (no longer disjoint), carry the
// original fix, and the poisoned root must be gone from the branch tip.
func TestRegraftOntoBaseEscapesDisjointHistory(t *testing.T) {
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
	git("config", "commit.gpgsign", "false")
	write("base.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "main")

	// Build a DISJOINT branch (orphan root, no shared history) that carries the
	// same base file plus a verified fix — the BEH-500 shape, where the content
	// diff against origin/main is just the real fix despite the disjoint root.
	git("checkout", "-q", "--orphan", branchName("beh-500"))
	git("rm", "-rfq", "--cached", ".")
	write("base.txt", "base\n")
	write("fix.txt", "the verified fix\n")
	git("add", "-A")
	git("commit", "-q", "-m", "handoff: the verified fix")
	if !realWorktree(repo, repo, "beh-500").IsDisjointFrom("origin/main") {
		t.Fatal("fixture precondition: the orphan branch must be disjoint from origin/main")
	}

	if err := realWorktree(repo, repo, "beh-500").RegraftOntoBase("origin/main"); err != nil {
		t.Fatalf("RegraftOntoBase: %v", err)
	}

	// After the regraft the branch shares origin/main's history (no longer disjoint).
	if realWorktree(repo, repo, "beh-500").IsDisjointFrom("origin/main") {
		t.Fatal("branch is still disjoint after regraft — the re-root did not take")
	}
	// origin/main is now an ancestor of the branch tip (a clean rebase target).
	if !realWorktree(repo, repo, "beh-500").IsRebasedOnto("origin/main") {
		t.Fatal("origin/main must be an ancestor of the regrafted tip")
	}
	// The verified fix survived the regraft.
	fix, err := exec.Command("git", "-C", repo, "show", "HEAD:fix.txt").Output()
	if err != nil || strings.TrimSpace(string(fix)) != "the verified fix" {
		t.Fatalf("regrafted tip must carry the verified fix; got %q err=%v", string(fix), err)
	}
	// And the base file from origin/main is present (faithful tree, not just the patch).
	if _, err := exec.Command("git", "-C", repo, "show", "HEAD:base.txt").Output(); err != nil {
		t.Fatalf("regrafted tip must carry origin/main's base.txt: %v", err)
	}
}

// BEH-609: regrafting a branch whose content already matches the base is a no-op
// with nothing to recover — RegraftOntoBase must error rather than create an empty
// commit, so the caller never mistakes "nothing to regraft" for a recovered diff.
func TestRegraftOntoBaseEmptyDiffErrors(t *testing.T) {
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
	git("config", "commit.gpgsign", "false")
	write("base.txt", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "main")

	// An orphan branch with the SAME tree as main: disjoint history, but zero content
	// diff against origin/main.
	git("checkout", "-q", "--orphan", branchName("beh-500-empty"))
	git("add", "-A")
	git("commit", "-q", "-m", "identical tree, unrelated root")
	if err := realWorktree(repo, repo, "beh-500-empty").RegraftOntoBase("origin/main"); err == nil {
		t.Fatal("RegraftOntoBase must error when there is no content diff to regraft")
	}
}

// BEH-603: BranchDiffEmpty reports whether the worktree's committed tip makes ZERO
// net change against origin/main (an empty `git diff origin/main`) — the
// empty-commit branch the harness wrongly opened as PR #642. `git diff --quiet`
// exits 0 when there is no diff and non-zero when there is, so exit 0 → empty.
func TestBranchDiffEmptyTrueWhenNoDiff(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	wt := fakeWorktree("beh-603", runners{run: run})
	if !wt.DiffEmpty() {
		t.Fatal("exit 0 from diff --quiet (no changes) should report the diff empty (true)")
	}
	got := strings.Join((*calls)[0], " ")
	want := "git -C " + wt.Path() + " diff --quiet origin/main"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestBranchDiffEmptyFalseWhenDiffPresent(t *testing.T) {
	// A non-zero exit means `diff --quiet` found changes → NOT empty.
	run, _ := scriptedRunner(1, errors.New("exit status 1"))
	if fakeWorktree("beh-603", runners{run: run}).DiffEmpty() {
		t.Fatal("a non-zero diff --quiet exit (changes present) should report not-empty (false)")
	}
}

// Fail-safe: any error other than the clean exit-0 (e.g. an unresolvable ref) must
// read as NOT empty, so the harness never recommends closing a ticket on doubt — it
// would rather attempt the push than wrongly advise a close.
func TestBranchDiffEmptyFalseOnError(t *testing.T) {
	run, _ := scriptedRunner(1, errors.New("fatal: bad revision 'origin/main'"))
	if fakeWorktree("beh-603", runners{run: run}).DiffEmpty() {
		t.Fatal("a git error must read as not-empty (fail-safe) so a close is never recommended on doubt")
	}
}

// Pins the real-git contract: an empty-commit branch (a commit with no tree change
// against origin/main, the BEH-365 artifact) reads as an empty diff; a branch with a
// real file change does not.
func TestBranchDiffEmptyAgainstRealGit(t *testing.T) {
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

	// An empty-commit branch: a commit ahead of main that changes nothing.
	git("checkout", "-q", "-b", branchName("beh-603-empty"))
	git("commit", "-q", "--allow-empty", "-m", "document finding (no functional change)")
	if !realWorktree(repo, repo, "beh-603-empty").DiffEmpty() {
		t.Fatal("an empty-commit branch makes no net change against origin/main — must read as empty diff")
	}

	// A real change on the branch is NOT an empty diff.
	write("feat.txt", "feat\n")
	git("add", "-A")
	git("commit", "-q", "-m", "real change")
	if realWorktree(repo, repo, "beh-603-empty").DiffEmpty() {
		t.Fatal("a branch with a committed file change must NOT read as an empty diff")
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
			git("checkout", "-q", "--orphan", branchName(slug))
		} else {
			git("checkout", "-q", "-b", branchName(slug))
		}
		write("feat.txt", "feat\n")
		git("add", "-A")
		git("commit", "-q", "-m", "handoff")
		return repo, slug
	}

	t.Run("orphan branch is disjoint", func(t *testing.T) {
		repo, slug := mkRepo(true)
		truth := realWorktree(repo, repo, slug).GroundTruth()
		if !truth.DisjointHistory {
			t.Errorf("an orphan branch (no common ancestor) must be flagged disjoint, got %+v", truth)
		}
		if truth.CommitsAhead < 1 {
			t.Errorf("a disjoint branch is still ahead by its own commits, got CommitsAhead=%d", truth.CommitsAhead)
		}
	})

	t.Run("normal feature branch is not disjoint", func(t *testing.T) {
		repo, slug := mkRepo(false)
		truth := realWorktree(repo, repo, slug).GroundTruth()
		if truth.DisjointHistory {
			t.Errorf("a feature branch off main shares a common ancestor — must NOT be flagged disjoint, got %+v", truth)
		}
	})
}

// AbortRebase restores a worktree a session left mid-replay to a clean state before
// the harness keeps it for a human. The conflict-resolution session now replays via
// `cherry-pick` (BEH-618), so a give-up can leave a cherry-pick in progress that
// `rebase --abort` does not clean — abortRebase must abort BOTH. Both are best-effort
// (no return): a no-op abort when neither is in progress fails harmlessly.
func TestAbortRebaseAbortsBothRebaseAndCherryPick(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	wt := fakeWorktree("beh-581", runners{run: run})
	wt.AbortRebase()
	var sawRebaseAbort, sawCherryAbort bool
	for _, c := range *calls {
		switch strings.Join(c, " ") {
		case "git -C " + wt.Path() + " rebase --abort":
			sawRebaseAbort = true
		case "git -C " + wt.Path() + " cherry-pick --abort":
			sawCherryAbort = true
		}
	}
	if !sawRebaseAbort {
		t.Errorf("expected a `git rebase --abort`, calls: %v", *calls)
	}
	if !sawCherryAbort {
		t.Errorf("expected a `git cherry-pick --abort` (BEH-618: the session now replays via cherry-pick), calls: %v", *calls)
	}
}

func TestStripWorktreePathsRemovesEachDeclaredPath(t *testing.T) {
	wt := t.TempDir()
	for _, dir := range []string{
		filepath.Join(wt, "web", "node_modules", "@oxlint", "binding-linux-arm64-gnu"),
		filepath.Join(wt, "obj", "Debug"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	if err := realWorktree(wt, wt, "beh-412").StripPaths([]string{"web/node_modules", "obj"}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	for _, gone := range []string{"web/node_modules", "obj"} {
		if _, err := os.Stat(filepath.Join(wt, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone, stat err = %v", gone, err)
		}
	}
}

// Idempotent: a worktree whose dependencies were never installed (or that was
// already stripped) must not be an error — the strip runs unconditionally on
// every handoff.
func TestStripWorktreePathsIsNoOpWhenAbsent(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "web"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := realWorktree(wt, wt, "beh-412").StripPaths([]string{"web/node_modules"}); err != nil {
		t.Fatalf("expected no error when the path is absent, got %v", err)
	}
}

// A Consumer whose toolchain leaves nothing platform-specific behind (Go) declares
// no strip paths, and the handoff must then touch the worktree at all (BEH-641).
func TestStripWorktreePathsWithNoDeclarationStripsNothing(t *testing.T) {
	wt := t.TempDir()
	src := filepath.Join(wt, "main.go")
	if err := os.WriteFile(src, []byte("package main"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := realWorktree(wt, wt, "beh-412").StripPaths(nil); err != nil {
		t.Fatalf("expected no error with no declared paths, got %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("an empty declaration must strip nothing, stat err = %v", err)
	}
}

// The strip is surgical: only the declared paths go — the committed source the
// reviewer is here to read stays put.
func TestStripWorktreePathsLeavesSourceIntact(t *testing.T) {
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

	if err := realWorktree(wt, wt, "beh-412").StripPaths([]string{"web/node_modules"}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("web/src/app.tsx should survive the strip, stat err = %v", err)
	}
}

// The paths are Consumer config, so a stray value must never reach outside the
// worktree. An absolute path, a `../` escape, and a root-resolving entry are all
// refused BEFORE any removal — this is an rm -rf driven by a committed file.
func TestStripWorktreePathsRefusesPathsOutsideTheWorktree(t *testing.T) {
	for name, rel := range map[string]string{
		"absolute":      "/etc",
		"parent escape": "../sibling",
		"worktree root": ".",
		"root via dots": "web/..",
	} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			wt := filepath.Join(parent, "wt")
			sibling := filepath.Join(parent, "sibling")
			for _, d := range []string{wt, sibling} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			canary := filepath.Join(wt, "keep.txt")
			if err := os.WriteFile(canary, []byte("x"), 0o644); err != nil {
				t.Fatalf("setup: %v", err)
			}

			if err := realWorktree(wt, wt, "beh-412").StripPaths([]string{rel}); err == nil {
				t.Errorf("StripWorktreePaths(%q) must be refused, got nil error", rel)
			}
			if _, err := os.Stat(sibling); err != nil {
				t.Errorf("the sibling directory must survive, stat err = %v", err)
			}
			if _, err := os.Stat(canary); err != nil {
				t.Errorf("the worktree must survive, stat err = %v", err)
			}
		})
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
	if realWorktree(repo, repo, slug).BranchExists() {
		t.Fatal("BranchExists should be false before feat/<slug> is created")
	}

	gitInRepo("branch", branchName(slug))
	if !realWorktree(repo, repo, slug).BranchExists() {
		t.Error("BranchExists should be true once feat/<slug> resolves")
	}

	// An unrelated slug must not resolve — the gate is keyed to the exact branch.
	if realWorktree(repo, repo, "beh-999-nope").BranchExists() {
		t.Error("BranchExists must not report a branch that was never created")
	}

	// A path that isn't a git repo makes rev-parse fail; the documented contract
	// treats any git failure as "ref absent" (false), never a panic or true.
	if realWorktree(t.TempDir(), t.TempDir(), slug).BranchExists() {
		t.Error("BranchExists must read an unreadable ref (non-repo path) as absent")
	}
}

func TestPushUsesNoVerifyAndCorrectArgs(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(0, nil)
	if err := fakeWorktree("beh-403", runners{run: run, sleep: clock.sleep, now: clock.now}).Push(); err != nil {
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
	if err := fakeWorktree("beh-570", runners{run: run, sleep: clock.sleep, now: clock.now}).PushForceWithLease(); err != nil {
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
	if err := fakeWorktree("beh-570", runners{run: run, sleep: clock.sleep, now: clock.now}).PushForceWithLease(); err != nil {
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
	if err := fakeWorktree("beh-403", runners{run: run, sleep: clock.sleep, now: clock.now}).Push(); err != nil {
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
	if err := fakeWorktree("beh-403", runners{run: run, sleep: clock.sleep, now: clock.now}).Push(); err != nil {
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
	err := fakeWorktree("beh-403", runners{run: run, sleep: clock.sleep, now: clock.now}).Push()
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
	err := fakeWorktree("beh-475", runners{run: run, sleep: sleep, now: clock.now}).Push()
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
	if err := fakeCheckout(runners{run: run, sleep: clock.sleep, now: clock.now}).FetchMain(); err != nil {
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
	if err := fakeCheckout(runners{sleep: sleep, now: clock.now}).withRetry(func() error { return errors.New("always") }); err == nil {
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
	if err := fakeCheckout(runners{sleep: sleep, now: clock.now}).withRetry(func() error { return nil }); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if sleeps != 0 {
		t.Fatalf("expected no sleeps on immediate success, got %d", sleeps)
	}
}

// consumerRoots is the excluded-root list a Consumer declares in its config — here,
// herd's. The classifier used to hardcode exactly this list; making it a fixture is
// the point of the change, so a Consumer with a different layout is a different
// argument rather than a fork of the harness.
var consumerRoots = []string{
	"web/", "agent-harness/", ".agents/", ".claude/",
	".github/", "scripts/", "supabase/", "infra/",
}

// BEH-687: DocsOnlyPaths recognises a diff that touches ONLY documentation/prose
// paths no build gate or CI job reads — the condition under which the host gate
// re-run and the CI poll can be skipped. The concrete case is a root-markdown edit
// (AGENTS.md, which CLAUDE.md symlinks to) plus an ADR under docs/.
func TestDocsOnlyPathsTrueForRootMarkdownAndDocs(t *testing.T) {
	if !DocsOnlyPaths([]string{"AGENTS.md", "docs/adr/0026-comments.md"}, consumerRoots) {
		t.Fatal("a diff of only root markdown + a docs/ ADR must be docs-only")
	}
}

// A single code path anywhere in the diff disqualifies the whole set: the change
// can affect a gate, so it must NOT short-circuit.
func TestDocsOnlyPathsFalseWhenAnyCodePathPresent(t *testing.T) {
	if DocsOnlyPaths([]string{"AGENTS.md", "web/src/routes/call/index.tsx"}, consumerRoots) {
		t.Fatal("a diff that also touches web/src is not docs-only")
	}
}

// An empty change set is not docs-only — a branch with nothing to ship is the
// zero-net-diff case (BEH-602), classified elsewhere.
func TestDocsOnlyPathsFalseWhenEmpty(t *testing.T) {
	if DocsOnlyPaths(nil, consumerRoots) {
		t.Fatal("an empty diff must not classify as docs-only")
	}
}

// Conservative exclusion: markdown INSIDE a module source tree (a README fixture, a
// Storybook doc) may feed a gate, so it is never treated as inert even though it is
// prose. Only prose OUTSIDE the source trees short-circuits.
func TestDocsOnlyPathsFalseForMarkdownUnderSourceTree(t *testing.T) {
	for _, p := range []string{"web/test/integration/README.md", "agent-harness/docs/DESIGN.md"} {
		if DocsOnlyPaths([]string{p}, consumerRoots) {
			t.Fatalf("%q lives under a module source tree — must not classify as docs-only", p)
		}
	}
}

// A skill markdown edit is NOT inert: the agent-harness internal/skills contract
// tests read the SKILL.md files, so `.agents/skills/**` (and its `.claude/` symlink
// form) triggers the Agent Harness CI job (agent-harness.yaml) and can turn it red.
// Classifying it docs-only would short-circuit a watch that could fail.
func TestDocsOnlyPathsFalseForSkillMarkdown(t *testing.T) {
	for _, p := range []string{
		".agents/skills/review-worktree/SKILL.md",
		".claude/skills/retrospective/SKILL.md",
	} {
		if DocsOnlyPaths([]string{p}, consumerRoots) {
			t.Fatalf("%q feeds the agent-harness skill-contract CI tests — must not classify as docs-only", p)
		}
	}
}

// Non-markdown gate/CI inputs (scripts with sibling tests, workflows, package
// manifests, migrations) must never read as inert prose.
func TestDocsOnlyPathsFalseForGateInputs(t *testing.T) {
	for _, p := range []string{
		"scripts/new-worktree.sh",
		".github/workflows/main.yaml",
		"web/package.json",
		"supabase/migrations/20260101000000_x.sql",
	} {
		if DocsOnlyPaths([]string{p}, consumerRoots) {
			t.Fatalf("%q is a gate/CI input — must not classify as docs-only", p)
		}
	}
}

// A non-markdown file under the repo-root docs/ tree (a diagram, an image) still
// feeds no gate, so a docs/-only diff is inert.
func TestDocsOnlyPathsTrueForNonMarkdownUnderDocs(t *testing.T) {
	if !DocsOnlyPaths([]string{"docs/adr/assets/flow.png"}, consumerRoots) {
		t.Fatal("a non-md asset under docs/ feeds no gate — must be docs-only")
	}
}

// scriptedOutput returns an outputRunner that yields fixed stdout + err, recording
// the argv it saw — the output-capturing sibling of scriptedRunner.
func scriptedOutput(stdout string, err error) (outputRunner, *[][]string) {
	var calls [][]string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(stdout), err
	}
	return run, &calls
}

// branchDocsOnly reads `git diff --name-only origin/main` and classifies the paths:
// an all-docs diff is docs-only.
func TestBranchDocsOnlyTrueForAllDocsDiff(t *testing.T) {
	run, calls := scriptedOutput("AGENTS.md\ndocs/adr/0026.md\n", nil)
	wt := fakeWorktree("beh-687", runners{output: run})
	if !wt.DocsOnly(consumerRoots) {
		t.Fatal("a name-only diff of pure docs must classify as docs-only")
	}
	got := strings.Join((*calls)[0], " ")
	want := "git -C " + wt.Path() + " diff --name-only origin/main"
	if got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestBranchDocsOnlyFalseWhenCodeChanged(t *testing.T) {
	run, _ := scriptedOutput("AGENTS.md\nweb/src/app.tsx\n", nil)
	if fakeWorktree("beh-687", runners{output: run}).DocsOnly(consumerRoots) {
		t.Fatal("a diff that touches web/src must not classify as docs-only")
	}
}

// Fail-safe: an empty name-only diff (the zero-net-diff case, handled elsewhere) is
// not a docs-only ship — falling back to false keeps the normal gate + watch.
func TestBranchDocsOnlyFalseWhenDiffEmpty(t *testing.T) {
	run, _ := scriptedOutput("\n", nil)
	if fakeWorktree("beh-687", runners{output: run}).DocsOnly(consumerRoots) {
		t.Fatal("an empty name-only diff must not classify as docs-only")
	}
}

// Fail-safe: any git error (unresolvable ref, gone worktree) reads as NOT docs-only,
// so the harness never skips the gate + watch on doubt.
func TestBranchDocsOnlyFalseOnError(t *testing.T) {
	run, _ := scriptedOutput("", errors.New("fatal: bad revision 'origin/main'"))
	if fakeWorktree("beh-687", runners{output: run}).DocsOnly(consumerRoots) {
		t.Fatal("a git error must read as not-docs-only (fail-safe)")
	}
}

// Pins the real-git contract: a branch whose only change against origin/main is a
// root-markdown edit reads as docs-only; adding a web/src file flips it off.
func TestBranchDocsOnlyAgainstRealGit(t *testing.T) {
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
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "Test")
	write("AGENTS.md", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "main")

	git("checkout", "-q", "-b", branchName("beh-687-docs"))
	write("AGENTS.md", "base\nnew guidance line\n")
	git("add", "-A")
	git("commit", "-q", "-m", "docs: add guidance")
	if !realWorktree(repo, repo, "beh-687-docs").DocsOnly(consumerRoots) {
		t.Fatal("a branch whose only change is a root-markdown edit must be docs-only")
	}

	write("web/src/app.tsx", "export const x = 1\n")
	git("add", "-A")
	git("commit", "-q", "-m", "feat: add code")
	if realWorktree(repo, repo, "beh-687-docs").DocsOnly(consumerRoots) {
		t.Fatal("adding a web/src file must flip the branch off docs-only")
	}
}

// A Consumer that declares no excluded roots gets NO short-circuit, not a
// short-circuit over everything. This is the direction that matters: the roots
// list is what stops the classifier calling a Consumer's own source-tree prose
// inert, so an absent list must fail closed. Reading it the other way — empty
// means exclude nothing, so every .md is inert — would make a fresh Consumer skip
// the CI watch on a README its own workflow triggers a job for, silently merging
// past a job that could go red (BEH-641).
func TestDocsOnlyPathsFalseWhenConsumerDeclaresNoRoots(t *testing.T) {
	for _, roots := range [][]string{nil, {}} {
		if DocsOnlyPaths([]string{"AGENTS.md", "docs/adr/0026.md"}, roots) {
			t.Errorf("with roots=%v a pure-docs diff must NOT be docs-only: an undeclared "+
				"root list disables the short-circuit rather than widening it", roots)
		}
	}
}

// The roots are the Consumer's, so the same path classifies differently for
// different Consumers — which is the whole reason the list stopped being a harness
// constant. A .NET Consumer excluding src/ must not have its src/ prose called
// inert, while the same path IS inert for a Consumer that never declared src/.
func TestDocsOnlyPathsHonoursTheConsumersOwnRoots(t *testing.T) {
	dotnet := []string{"src/", ".github/"}
	if DocsOnlyPaths([]string{"src/Api/README.md"}, dotnet) {
		t.Error("src/Api/README.md is under a declared root — must not be docs-only for this Consumer")
	}
	if !DocsOnlyPaths([]string{"src/Api/README.md"}, consumerRoots) {
		t.Error("herd declares no src/ root, so the same path is inert prose for herd — " +
			"the classifier must read the Consumer's list, not a harness constant")
	}
}
