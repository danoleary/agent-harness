package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/danoleary/agent-harness/internal/proc"
)

// Checkout is the Consumer's primary checkout on the host: the directory the
// harness clones nothing into and mutates nothing in, but reads history from,
// fetches origin/main into, pushes branches out of, and hangs every ticket's
// worktree off. It binds the two values that used to travel as the first two
// thirds of the (herdPath, branchPrefix, slug) triple, so nothing below has to
// thread them again.
//
// It carries the package's one seam (see [runners]). [Open] builds the
// production Checkout; a test builds one over fakes and gets every operation in
// the package — including the mutating remote ones — without a remote.
type Checkout struct {
	path         string
	branchPrefix string
	runners
}

// Open binds the Consumer's checkout and its configured branch prefix (ADR-0008)
// to the real git binary, the real clock and the real sleeper.
func Open(path, branchPrefix string) Checkout {
	return Checkout{path: path, branchPrefix: branchPrefix, runners: execRunners()}
}

// Worktree names the worktree the harness creates for one ticket slug. It is a
// pure naming operation — the worktree need not exist yet (that is [Worktree.Create])
// and none of git runs here.
func (c Checkout) Worktree(slug string) Worktree {
	return Worktree{
		repo:    c.path,
		branch:  c.branchPrefix + "/" + slug,
		path:    filepath.Join(c.path, ".claude", "worktrees", slug),
		runners: c.runners,
	}
}

// Worktree is one ticket's worktree and the canonical branch checked out in it:
// the harness's central noun. It holds the three strings every git operation in
// the harness needs — the Consumer checkout it hangs off, the branch
// `<branchPrefix>/<slug>`, and the worktree's host path — so a caller names the
// ticket once and never re-derives them.
//
// Which of the three an operation uses is a fact about git, not about the caller:
// reads of the branch ref and both pushes run against the shared `.git` from the
// main checkout (the sandbox commits into it through the bind mount, so the ref
// and its objects are visible there), while reads and mutations of the working
// tree run against the worktree path, whose absolute `.git` pointer resolves on
// the host via the real-path mount (ADR-0002). Both are now internal detail.
//
// A Worktree is a value: copying one is free and safe, and [Checkout.Worktree]
// mints them on demand.
type Worktree struct {
	repo   string // the Consumer's primary checkout
	branch string // <branchPrefix>/<slug>
	path   string // <repo>/.claude/worktrees/<slug>
	runners
}

// Path is the host path of the worktree. Callers that must hand the path to
// something outside this package — a bind mount, a container's working dir —
// read it here; nothing inside the package needs it any more.
func (w Worktree) Path() string { return w.path }

// Branch is the canonical feature branch the harness creates for this ticket:
// the Consumer's configured branch prefix joined to the slug (ADR-0008/BEH-636).
// The harness owns this branch host-side and keys verify/push/PR/dispatch-guards
// off it, so the prefix is no longer the hardcoded "feat" — a Consumer that sets
// branch_prefix = "fix" gets `fix/<slug>`.
func (w Worktree) Branch() string { return w.branch }

// Exists reports whether the worktree directory is on disk — the implementation
// stage's "already provisioned?" check and the review stage's precondition
// ("run `implementation <ticket>` first").
func (w Worktree) Exists() bool {
	_, err := os.Stat(w.path)
	return err == nil
}

// Create creates the feature worktree + canonical branch host-side
// (ADR-0008/BEH-636), retiring the old coupling where the sandbox agent ran the
// Consumer's `new-worktree.sh` and the host depended on its output. It adds a
// `git worktree add -b <branchPrefix>/<slug>` at the worktree path, based on the
// freshly-fetched origin/main (falling back to local `main` when the remote-
// tracking ref doesn't resolve). Toolchain provisioning inside the created
// worktree is the Consumer's `post_create` hook, run separately by the caller —
// this does pure git only, so it is language-agnostic.
func (w Worktree) Create() error {
	// Resumed-branch case (BEH-554): the branch already exists from a prior session
	// but its worktree dir was torn down. Re-attach a worktree to it (no `-b`, which
	// would fail "branch already exists") so the prior work is preserved.
	if w.run("git", "-C", w.repo, "rev-parse", "--verify", "-q", "refs/heads/"+w.branch) == nil {
		return w.run("git", "-C", w.repo, "worktree", "add", w.path, w.branch)
	}
	// Fresh branch: root it at current origin/main. Best-effort refresh — an
	// offline/blipping remote just means we branch off whatever ref we already have.
	_ = w.run("git", "-C", w.repo, "fetch", "-q", "origin", "main")
	base := "main"
	if w.run("git", "-C", w.repo, "rev-parse", "--verify", "-q", "refs/remotes/origin/main") == nil {
		base = "refs/remotes/origin/main"
	}
	return w.run("git", "-C", w.repo, "worktree", "add", "-b", w.branch, w.path, base)
}

// Remove tears down the worktree at `.claude/worktrees/<slug>` from the main
// checkout. The real-path bind mount (ADR-0002) makes the worktree's absolute
// `.git` pointer resolve on the host, so no throwaway container is needed. It is
// deliberately not forced: if the worktree still holds uncommitted work, git
// refuses and the harness keeps it (a recoverable artifact) rather than nuking
// unpushed changes.
func (w Worktree) Remove() error {
	return w.run("git", "-C", w.repo, "worktree", "remove", w.path)
}

// HeadSHA returns the commit SHA at the worktree's HEAD, read host-side via the
// real-path mount (the same seam Clean/Checkpoint use). It lets a caller capture
// the tip before a session and tell afterwards whether that session actually
// committed anything.
func (w Worktree) HeadSHA() (string, error) {
	out, err := w.output("git", "-C", w.path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CommitSubjects returns the subject lines of the commits on the feature branch
// ahead of origin/main, newest last — the raw material for the templated PR body.
// Read from the main checkout (the shared `.git` holds the branch's objects); an
// empty result on any git failure keeps the caller crash-free.
func (w Worktree) CommitSubjects() []string {
	out, err := w.output("git", "-C", w.repo, "log", "--reverse", "--format=%s", "origin/main.."+w.branch)
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

// Clean reports whether the worktree has no uncommitted changes — the guarantee
// that the tree the host-side gate validated is exactly the tree that [Worktree.Push]
// ships. The gate runs against the worktree's working files (committed +
// uncommitted), but the push ships only the committed branch tip; a dirty worktree
// (e.g. a review session that edited but never committed) would mean the gate
// validated a different tree than would ship. Read host-side via the real-path
// mount; any git failure is treated as not-clean (fail safe — never push on doubt).
func (w Worktree) Clean() bool {
	out, err := w.output("git", "-C", w.path, "status", "--porcelain")
	if err != nil {
		return false
	}
	return !porcelainHasRealChanges(string(out))
}

// Rebase replays the worktree's feature branch onto origin/main so the PR opens on
// a current base instead of the stale one a long pipeline accreted (a sibling PR
// merging underneath it — BEH-570). origin/main must already be fresh (call
// [Checkout.FetchMain] first) and the worktree clean. Run host-side against the
// worktree, whose absolute .git resolves via the real-path mount (ADR-0002) — the
// same seam Clean/Checkpoint use.
//
// The replay deliberately avoids `git rebase`: in a linked worktree (every harness
// worktree is a `git worktree add` checkout) rebase's detach-to-onto full-tree
// checkout false-fails with "local changes would be overwritten by merge" / "could
// not detach HEAD" even on a byte-clean tree — a known worktree checkout-safety
// artifact (BEH-618). It instead does `reset --hard origin/main` (the same full-tree
// move, but without the false-positive) then `cherry-pick`s the feature commits back
// on. cherry-pick's three-way merge surfaces only genuine content conflicts.
//
// A clean replay returns RebaseClean. A genuine conflict (or git refusing for any
// other reason) aborts the cherry-pick, restores the branch to its original tip, and
// returns RebaseConflict, so the caller hands resolution to a human rather than
// pushing a branch it could not cleanly replay. There is no network here, so unlike
// the remote ops it is not retried.
func (w Worktree) Rebase() RebaseResult {
	// Strip the readiness sentinel before replaying. new-worktree.sh drops an untracked
	// .worktree-ready into every worktree (BEH-549), and a checkout that wants to write a
	// now-tracked .worktree-ready over the untracked copy can refuse ("untracked working
	// tree files would be overwritten") — the BEH-617 trap. Best-effort: a no-op (os
	// error) when the sentinel is absent.
	_ = os.Remove(filepath.Join(w.path, WorktreeReadySentinel))

	// Pin the feature tip so `reset --hard` can't lose the commits and the cherry-pick
	// range / conflict-restore can name them.
	if w.run("git", "-C", w.path, "update-ref", rebaseBackupRef, "HEAD") != nil {
		return RebaseConflict
	}
	defer func() { _ = w.run("git", "-C", w.path, "update-ref", "-d", rebaseBackupRef) }()

	// Move the branch onto the fresh base. reset --hard escapes the rebase checkout
	// false-fail and also fast-forwards a branch that is merely behind origin/main.
	if w.run("git", "-C", w.path, "reset", "--hard", "origin/main") != nil {
		return RebaseConflict
	}

	// Nothing to replay if the feature tip is already contained in origin/main (the
	// branch was behind or equal): the reset alone completed the move. cherry-pick of
	// an empty range errors, so short-circuit it.
	if w.run("git", "-C", w.path, "merge-base", "--is-ancestor", rebaseBackupRef, "origin/main") == nil {
		return RebaseClean
	}

	// Replay the feature's own commits (origin/main..tip) onto the new base. cherry-pick
	// rewrites the COMMITTER of every replayed commit to whoever runs it, so stamp the
	// harness identity — exactly as the old rebase did — so the pushed branch never
	// inherits the host checkout's placeholder identity (BEH-579).
	//
	// A bare cherry-pick (no `--empty=drop`) is used deliberately for git-version
	// portability: `--empty=<action>` for cherry-pick only landed in git 2.45 (May
	// 2024), and CI runners (and older host checkouts) still ship git 2.39, where the
	// flag is rejected with a usage error (exit 129) that fails EVERY clean replay.
	// Both empty-commit classes are instead handled uniformly by the auto-skip loop
	// below, which reproduces an ideal `git rebase`'s auto-drop without the flag.
	args := append([]string{"-C", w.path}, identityArgs()...)
	args = append(args, "cherry-pick", "origin/main.."+rebaseBackupRef)
	if w.run("git", args...) == nil {
		return RebaseClean
	}

	// The cherry-pick halted. A bare cherry-pick halts on BOTH empty-commit classes
	// with "the previous cherry-pick is now empty" (exit 1): a commit that BECOMES
	// empty on replay because its diff is already in origin/main (a sibling PR merged
	// the same change, or a hotfix was cherry-picked to main — the BEH-622 case), AND
	// a commit that was INITIALLY empty — an `--allow-empty` handoff commit with a zero
	// net diff (the BEH-678 d0177234b case). Neither is a content conflict: dropping
	// either is a no-op, so escalating to a sandboxed conflict-resolution session
	// (BEH-581) would waste a whole session on an empty commit. Auto-skip both inline
	// instead, matching an ideal `git rebase`'s auto-drop. The empty-halt classes are
	// told apart from a genuine conflict by unmerged paths: a conflict leaves unmerged
	// index entries; an empty halt leaves a clean index. Loop because a multi-commit
	// replay can halt on several empty commits in turn; each `--skip` consumes one, so
	// it always makes progress and terminates.
	//
	// Fail safe first: a cherry-pick error that left NO pick in progress is an
	// unexpected failure (not a halt on an empty commit or a conflict), so restore the
	// branch and report RebaseConflict rather than fall through to RebaseClean and push
	// an incompletely-replayed branch — the file's "never push on doubt" convention.
	if !w.cherryPickInProgress() {
		_ = w.run("git", "-C", w.path, "cherry-pick", "--abort")
		_ = w.run("git", "-C", w.path, "reset", "--hard", rebaseBackupRef)
		return RebaseConflict
	}
	for i := 0; w.cherryPickInProgress(); i++ {
		if w.hasUnmergedPaths() || i >= maxCherryPickSkips {
			// Genuine content conflict (or a pathological non-progressing skip loop — the
			// bound is a pure safety backstop): abort the cherry-pick and restore the
			// branch to its original tip, leaving it exactly as it was for a human
			// (BEH-570). Both are best-effort — if the cherry-pick never started, --abort
			// fails harmlessly.
			_ = w.run("git", "-C", w.path, "cherry-pick", "--abort")
			_ = w.run("git", "-C", w.path, "reset", "--hard", rebaseBackupRef)
			return RebaseConflict
		}
		// Empty commit → drop it and continue the sequence. --skip may itself halt on the
		// next commit (empty or conflict), so its exit code isn't trusted — the loop
		// re-reads git state and re-classifies.
		_ = w.run("git", "-C", w.path, "cherry-pick", "--skip")
	}
	return RebaseClean
}

// cherryPickInProgress reports whether a cherry-pick is currently halted awaiting
// resolution — `git rev-parse --verify --quiet CHERRY_PICK_HEAD` exits 0 iff
// CHERRY_PICK_HEAD resolves. Used to drive the auto-skip loop over initially-empty
// commits (BEH-678). Run host-side against the worktree via the real-path mount.
func (w Worktree) cherryPickInProgress() bool {
	return w.run("git", "-C", w.path, "rev-parse", "--verify", "--quiet", "CHERRY_PICK_HEAD") == nil
}

// hasUnmergedPaths reports whether the worktree's index carries unmerged (conflicted)
// entries — `git diff --quiet --diff-filter=U` exits 0 when there are none and
// non-zero when there are. It is what tells a genuine content conflict (unmerged
// entries) apart from a halted-but-empty cherry-pick (clean index) so the harness
// auto-skips the latter and only escalates the former (BEH-678). Run host-side
// against the worktree via the real-path mount.
func (w Worktree) hasUnmergedPaths() bool {
	return w.run("git", "-C", w.path, "diff", "--quiet", "--diff-filter=U") != nil
}

// AbortRebase restores a worktree a conflict-resolution session left mid-replay to a
// clean, on-branch state before the harness keeps it for a human (BEH-581). The
// session replays via `cherry-pick` (BEH-618), so a give-up can strand a cherry-pick
// in progress that `rebase --abort` won't clean — abort BOTH. Best-effort: if neither
// is in progress the aborts fail harmlessly, so there is nothing to surface — callers
// fire it unconditionally on a failed resolution.
func (w Worktree) AbortRebase() {
	_ = w.run("git", "-C", w.path, "rebase", "--abort")
	_ = w.run("git", "-C", w.path, "cherry-pick", "--abort")
}

// IsRebasedOnto reports whether ref (e.g. "origin/main") is an ancestor of the
// worktree's HEAD — i.e. the branch genuinely contains the latest base. It is the
// ground-truth guard the pre-push conflict-resolution path needs (BEH-581): a
// sandboxed session that gives up and runs `git rebase --abort` leaves a CLEAN
// worktree on the original stale tip, so Clean alone would wave it through.
// Confirming the rebase actually landed stops the harness re-gating and pushing a
// still-stale branch. Read host-side via the real-path mount; any git failure (the
// non-ancestor exit, or git refusing) reads as not-rebased.
func (w Worktree) IsRebasedOnto(ref string) bool {
	return w.run("git", "-C", w.path, "merge-base", "--is-ancestor", ref, "HEAD") == nil
}

// IsDisjointFrom reports whether the worktree's HEAD shares NO common ancestor
// with ref (e.g. "origin/main") — the disjoint-history condition (BEH-597) that
// passed the tdd gate as a 963-commits-ahead "success" in BEH-355. `git
// merge-base <ref> HEAD` exits 0 and prints the common ancestor when one exists;
// it exits non-zero with empty output when the histories are disjoint. The
// pre-push rebase needs this to avoid mislabelling the inevitable replay-collision
// of a disjoint branch as a content conflict and burning a sandboxed
// conflict-resolution session on the wrong problem. Read host-side via the
// real-path mount; at the call site both refs resolve, so any non-zero exit means
// no common ancestor (disjoint), not an unresolved ref.
func (w Worktree) IsDisjointFrom(ref string) bool {
	return branchesDisjoint(w.run, w.path, ref, "HEAD")
}

// RegraftOntoBase rescues verified work trapped on a DISJOINT branch (BEH-609).
// When a tdd session's handoff lands on a branch that roots at an unrelated history
// (an empty `git merge-base` with origin/main — the BEH-355/BEH-500 condition), the
// gate fails it and re-launching can never escape it: no in-sandbox work changes
// the branch's root commit. So the harness re-roots the work host-side — it captures
// the branch's CONTENT diff against ref (two-dot, a direct tree comparison, so it
// works ACROSS the disjoint root where `git diff ref...HEAD` would die with "no merge
// base"), resets the branch onto ref, and re-applies the diff as one fresh commit.
// The result shares ref's history (rebaseable, handoff-able) while preserving the
// exact verified tree. The original handoff message is carried over with a regraft
// note; the disjoint tip stays in the reflog for forensics. Errors (leaving the
// branch reset onto ref) if there is no content diff — nothing to recover, never an
// empty commit. Run host-side against the worktree via the real-path mount (ADR-0002).
func (w Worktree) RegraftOntoBase(ref string) error {
	// Capture the content diff and handoff message BEFORE moving the branch.
	// --binary so binary blobs / mode / deletion changes regraft faithfully.
	patch, err := w.output("git", "-C", w.path, "diff", "--binary", ref, "HEAD")
	if err != nil {
		return fmt.Errorf("capturing content diff against %s: %w", ref, err)
	}
	if len(bytes.TrimSpace(patch)) == 0 {
		return fmt.Errorf("no content diff against %s — nothing to regraft", ref)
	}
	msg, err := w.output("git", "-C", w.path, "log", "-1", "--format=%B")
	if err != nil {
		return fmt.Errorf("reading handoff message: %w", err)
	}
	// Re-root: reset --hard moves the branch ref onto ref's tip (escaping the
	// disjoint root; the old tip survives in the reflog) and matches the worktree.
	if err := w.run("git", "-C", w.path, "reset", "--hard", ref); err != nil {
		return fmt.Errorf("resetting onto %s: %w", ref, err)
	}
	// Re-apply the captured diff onto the fresh base (index + worktree).
	if out, aerr := w.pipe(patch, "git", "-C", w.path, "apply", "--index"); aerr != nil {
		return fmt.Errorf("re-applying diff onto %s: %w (%s)", ref, aerr, strings.TrimSpace(string(out)))
	}
	// Commit the regrafted tree, preserving the handoff message with a regraft note.
	commitMsg := strings.TrimRight(string(msg), "\n") +
		"\n\nRe-grafted onto " + ref + " to escape a disjoint history (BEH-609)."
	args := append([]string{"-C", w.path}, identityArgs()...)
	args = append(args, "commit", "--no-verify", "-m", commitMsg)
	if err := w.run("git", args...); err != nil {
		return fmt.Errorf("committing regrafted diff: %w", err)
	}
	return nil
}

// DiffEmpty reports whether the worktree's committed tip makes ZERO net change
// against origin/main (an empty `git diff origin/main`) — the empty-commit branch
// the harness wrongly opened as PR #642 (BEH-603). It is the detection half of the
// recommend-close disposition: a clean, gate-green, reviewed branch with an empty
// diff has nothing to ship, so the ticket should be closed as a duplicate/superseded
// rather than opened as an empty-commit PR. Callers must check Clean first — `git
// diff origin/main` includes uncommitted changes, so on a dirty tree an "empty"
// committed diff could still hide real uncommitted work.
//
// `git diff --quiet` exits 0 when there is no diff and non-zero when there is, so
// only a clean exit-0 reports empty. Any error (a non-zero diff exit, or an
// unresolvable ref) reads as NOT empty — the fail-safe direction: the harness would
// rather attempt the push than wrongly recommend closing a ticket on doubt. Read
// host-side via the real-path mount, the same seam Clean/Rebase use.
func (w Worktree) DiffEmpty() bool {
	return w.run("git", "-C", w.path, "diff", "--quiet", "origin/main") == nil
}

// DocsOnly reports whether the branch's net diff against origin/main touches ONLY
// documentation/prose paths that no build gate or CI job reads (BEH-687) — the
// condition under which the review host-gate re-run and the CI poll can be skipped
// for a change that provably cannot break the build. It reads the changed paths with
// `git diff --name-only origin/main` and classifies them with [DocsOnlyPaths]. Like
// DiffEmpty it is fail-safe: a git error (unresolvable ref, gone worktree) or an
// empty diff (the zero-net-diff case, BEH-602) reports false, so the normal gate +
// watch still run — the harness would rather validate than wrongly skip. Read
// host-side via the real-path mount, the same seam DiffEmpty uses.
// excludedRoots is the Consumer's declared list of directories whose contents are
// never inert (ADR-0008). An empty list disables the short-circuit entirely — see
// [DocsOnlyPaths] for why that is the safe default.
func (w Worktree) DocsOnly(excludedRoots []string) bool {
	out, err := w.output("git", "-C", w.path, "diff", "--name-only", "origin/main")
	if err != nil {
		return false
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return DocsOnlyPaths(paths, excludedRoots)
}

// Push pushes the feature branch to origin from the main checkout (ADR-0002: the
// harness owns the push, host-side; the sandbox never reaches a remote). Run only
// after the harness's own gate re-run is green.
//
// --no-verify deliberately skips the host lefthook pre-push hook: the harness has
// already independently re-run the full gate in a throwaway Linux container
// (sandbox.BuildWorktreeCommandArgs) — that container's exit code is the sole
// authority for a push. The host hook is redundant duplication, and running it
// here is actively wrong: the push happens from the main checkout (HEAD=main, not
// the feature branch), so lefthook either silently skips every command (its
// push-file set is empty) or, if it ran, would build the wrong tree against
// host-platform node_modules the worktree doesn't have. Retried against transient
// remote blips under a wall-clock budget (BEH-329, widened in BEH-403).
func (w Worktree) Push() error {
	return w.withRetry(func() error {
		return w.run("git", "-C", w.repo, "push", "--no-verify", "origin", w.branch)
	})
}

// PushForceWithLease re-pushes the feature branch after an auto-rebase rewrote its
// history (BEH-570). It is needed only on the reactive path — a branch already on
// the remote whose tip the rebase moved — so a plain [Worktree.Push] would be
// rejected as non-fast-forward. --force-with-lease is the safe force: it refuses to
// overwrite remote commits the harness hasn't observed (it never will here — the
// harness owns the branch — but the lease is the correct, non-destructive force).
// --no-verify and the transient-retry budget match Push.
func (w Worktree) PushForceWithLease() error {
	return w.withRetry(func() error {
		return w.run("git", "-C", w.repo, "push", "--no-verify", "--force-with-lease", "origin", w.branch)
	})
}

// CommitsAhead is the number of commits on the feature branch ahead of
// origin/main — the ground truth that a finished tdd session actually left a
// handoff. The sandbox commits into the shared `.git` (bind-mounted), so the
// feature branch ref + objects are visible from the main checkout without touching
// the worktree itself — the harness never runs git inside a worktree for these
// reads (a worktree's `.git` pointer is container-relative), only against the main
// checkout. On any failure (branch doesn't exist / no upstream) it reads as zero.
func (w Worktree) CommitsAhead() int {
	out, err := w.output("git", "-C", w.repo, "rev-list", "--count", "origin/main.."+w.branch)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return n
}

// DisjointHistory reports whether the feature branch shares no common ancestor with
// origin/main (an empty merge-base — the BEH-355 condition). Like [Worktree.CommitsAhead]
// it reads the branch ref from the main checkout, so the worktree need not exist.
//
// It answers false for a branch that is not ahead, which is what makes it safe to
// call on its own: `git merge-base` also fails for an unresolvable ref, and without
// the ahead check an absent branch would read as "disjoint" rather than as the
// plain no-handoff failure it is (BEH-597). The cost is one extra `rev-list`, which
// is the right trade for a predicate no caller can hold wrong.
func (w Worktree) DisjointHistory() bool {
	return w.CommitsAhead() > 0 && branchesDisjoint(w.run, w.repo, w.branch, "origin/main")
}

// BranchExists reports whether the feature branch resolves to a git revision in the
// main checkout — i.e. the upstream implementation session actually created it. It
// reads the LOCAL head ref (`refs/heads/<branchPrefix>/<slug>`), where the sandbox's
// commits land via the shared `.git`; distinct from [Worktree.BranchPushed], which
// reads the remote-tracking ref. It is the retrospective's host-side precondition
// (BEH-553): a branch that doesn't resolve means there is no diff to retrospect. Any
// git failure → false (treat an unreadable ref as absent).
func (w Worktree) BranchExists() bool {
	return w.run("git", "-C", w.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+w.branch) == nil
}

// BranchPushed reports whether the feature branch reached origin, read from the main
// checkout's remote-tracking ref (the review stage's host-side push sets it). It is
// the safe gate on tearing down a worktree: the harness only removes a worktree whose
// branch is on the remote, so a teardown can never lose work that hasn't been pushed
// (DESIGN.md "On a clean run … git worktree remove").
func (w Worktree) BranchPushed() bool {
	return w.run("git", "-C", w.repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+w.branch) == nil
}

// StripPaths removes the Consumer-declared handoff-strip paths from the worktree the
// implementation stage hands back. The sandbox builds the worktree on linux-arm64
// (ADR-0002), so a dependency tree installed in there carries Linux-only native
// bindings; a reviewer running the gates on a non-Linux host hits a cryptic load
// failure that a frozen re-install won't repair (the platform-conditional optional
// deps look satisfied). Stripping those paths means the reviewer always installs
// fresh for their own platform, and the Consumer's post_create re-provisions them
// before the review session (BEH-412).
//
// Which paths those are is a Consumer choice, not a harness fact: `web/node_modules`
// for a pnpm monorepo, `obj/` + `bin/` for .NET, nothing at all for Go (BEH-641).
// Paths are relative to the worktree root; an absolute or parent-escaping path is
// refused so a stray config value can never delete outside the worktree.
// Idempotent: a no-op when a path is already absent, and when none are declared.
func (w Worktree) StripPaths(paths []string) error {
	for _, rel := range paths {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		if filepath.IsAbs(rel) {
			return fmt.Errorf("handoff_strip_paths entry %q must be relative to the worktree", rel)
		}
		target := filepath.Join(w.path, rel)
		// filepath.Join cleans the path, so a `../` escape shows up here as a target
		// that is no longer under the worktree.
		if target != w.path && !strings.HasPrefix(target, w.path+string(filepath.Separator)) {
			return fmt.Errorf("handoff_strip_paths entry %q escapes the worktree", rel)
		}
		if target == w.path {
			return fmt.Errorf("handoff_strip_paths entry %q resolves to the worktree root", rel)
		}
		if err := os.RemoveAll(target); err != nil {
			return err
		}
	}
	return nil
}

// Checkpoint captures whatever uncommitted work a finished session left in the
// worktree as a recovery commit on the feature branch, so a session that ended
// (wall-clock cap / usage-policy refusal / crash) before committing its work leaves
// a recoverable commit instead of a bare worktree that needs manual rescue.
// Both stages use it: the tdd session (BEH-479: the cap fired during a final
// verification re-run and discarded a finished diff) and the review session
// (BEH-559: a cap killed a review mid-nit-fix, and the in-progress edit was
// silently lost on resume — the resumed review then re-judged the original diff and
// flipped its verdict). `session` names which stage left the work so the message
// attributes it correctly. It is a SAFETY NET, not a verdict — the work is
// unverified, so the commit subject loudly marks it a harness checkpoint. Staging
// is `-A` (this is recovery: capture every change, tracked and untracked) and the
// commit is `--no-verify` (the work may not pass hooks — that is precisely why it
// is a checkpoint and not a handoff). A no-op success when the worktree is already
// clean (nothing was left behind to recover). Run host-side against the worktree
// via the real-path mount (ADR-0002), the same seam Clean uses.
func (w Worktree) Checkpoint(identifier, session string) error {
	if w.Clean() {
		return nil
	}
	if err := w.run("git", "-C", w.path, "add", "-A"); err != nil {
		return err
	}
	// Never let the readiness sentinel (BEH-549) ride into a recovery commit: on a
	// stale-base branch where it is not yet gitignored, `add -A` stages it. Unstaging
	// it keeps the checkpoint to real session work and stops the gitignored artifact
	// reattaching as a tracked file (BEH-612). Best-effort — a no-op when absent.
	_ = w.run("git", "-C", w.path, "reset", "-q", "--", WorktreeReadySentinel)
	// Belt-and-braces: refuse to commit when nothing real is staged. `git diff
	// --cached --quiet` exits 0 (nil) iff the index matches HEAD — so a checkpoint
	// can never capture zero work (e.g. a sentinel-only tree the guard let through).
	if w.run("git", "-C", w.path, "diff", "--cached", "--quiet") == nil {
		return nil
	}
	args := append([]string{"-C", w.path}, identityArgs()...)
	args = append(args, "commit", "--no-verify", "-m", CheckpointMessage(identifier, session))
	return w.run("git", args...)
}

// EnsureCIRerunCommit guarantees the just-finished auto-fix session leaves CI a
// fresh HEAD to run against. An auto-fix agent that reproduces every gate locally
// and finds no code defect (the red was a cancelled/superseded/flaky run) should
// NOT fabricate a speculative diff just to satisfy the loop's "produce a commit"
// contract — it leaves the worktree clean with HEAD unmoved. In that case the
// harness adds an empty commit so the re-push moves the branch tip and CI re-runs
// (clearing the stale red); if the agent did commit a real fix (HEAD moved), this
// is a no-op. The worktree must already be clean — the caller checks that and the
// empty commit ships only committed history (BEH-561).
func (w Worktree) EnsureCIRerunCommit(headBefore string) error {
	head, err := w.HeadSHA()
	if err != nil {
		return err
	}
	// HEAD moved → the agent committed a real fix; that is what re-triggers CI.
	if head != headBefore {
		return nil
	}
	args := append([]string{"-C", w.path}, identityArgs()...)
	args = append(args, "commit", "--no-verify", "--allow-empty", "-m", CIRerunMessage())
	return w.run("git", args...)
}

// FetchMain fast-forwards the primary checkout's view of origin/main so commit
// ranges and the PR base are current (DESIGN.md: pull origin/main after every
// session). Retried against transient remote blips; a fetch failure is returned
// for the caller to log, not fatal.
func (c Checkout) FetchMain() error {
	return c.withRetry(func() error {
		return c.run("git", "-C", c.path, "fetch", "-q", "origin", "main")
	})
}

// TicketAlreadyOnMain reports whether the ticket Key already appears in recent
// origin/main history — i.e. its work merged, so dispatching a fresh tdd session
// would burn a whole worktree + install only to discover an empty diff and raise
// no PR (BEH-528). It first refreshes origin/main with a single best-effort fetch
// (the host checkout's remote-tracking ref can lag a just-merged PR), then scans.
//
// The key is the tracker-agnostic Key (ADR-0010) the adapter produced — a Linear
// `BEH-123` or a Jira `PROJ-123` all the same; the scan makes no `BEH-`
// assumption, treating the Key as an opaque token to word-boundary-match.
//
// It fails OPEN: any git error (no remote, detached/corrupt checkout, a fetch
// blip) returns false so a flaky read never blocks a legitimate dispatch. The
// asymmetry is deliberate — a false negative costs one session (the pre-guard
// status quo), whereas a false positive would silently drop real work.
func (c Checkout) TicketAlreadyOnMain(key string) bool {
	// Best-effort refresh; an offline/blipping remote just means we scan whatever
	// origin/main we already have rather than block dispatch behind the network.
	_ = c.run("git", "-C", c.path, "fetch", "-q", "origin", "main")
	out, err := c.output("git", "-C", c.path, "log", "--oneline", "-"+strconv.Itoa(mainHistoryLookback), "origin/main")
	if err != nil {
		return false
	}
	return mainHistoryReferences(string(out), key)
}

// TicketHasRemoteBranch reports whether a branch referencing the ticket Key has been
// pushed to origin — the "work is in flight" signal the stale-claim reaper (BEH-677)
// checks alongside the linked-PR signal before releasing a claim. The harness names
// its feature branches `<prefix>/<slug>` where the slug carries the key, so a
// word-boundary match over `git ls-remote --heads` names finds it.
//
// It fails SAFE toward NOT reaping: any git error (no remote, a network blip)
// returns TRUE, so a flaky ls-remote can never cause a live claim to be released.
// The asymmetry with TicketAlreadyOnMain (which fails open to false) is deliberate —
// there a false positive drops work; here a false negative would reap live work, so
// each fails in its own safe direction.
func (c Checkout) TicketHasRemoteBranch(key string) bool {
	out, _, err := proc.OutputInDir(remoteOpTimeout, c.path, "git", "ls-remote", "--heads", "origin")
	if err != nil {
		return true
	}
	return remoteBranchesReference(string(out), key)
}
