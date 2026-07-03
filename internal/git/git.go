// Package git gathers the ground truth a finished tdd session leaves behind,
// read from the host's primary checkout.
package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/proc"
	"github.com/beherd/agent-harness/internal/verify"
)

// commandRunner runs a command to completion, returning only its error.
// Production uses execRun; tests inject a fake to drive the retry logic
// without touching a real remote (mirrors sandbox.Preflight's runner seam).
type commandRunner func(name string, args ...string) error

// HarnessAuthorName / HarnessAuthorEmail are the dedicated bot identity the
// harness stamps on every commit it is responsible for: the host-side recovery
// (CheckpointCommit) and CI-rerun (EnsureCIRerunCommit) commits it makes
// directly, the committer of the commits it rebases before pushing
// (RebaseOntoMain), and — via sandbox.BuildDockerRunArgs' GIT_AUTHOR_*/
// GIT_COMMITTER_* env — the agent's in-container handoff commit. Without it the
// sandbox checkout's placeholder `Test <test@example.com>` LOCAL git config —
// which overrides the entrypoint's `git config --global` identity — leaks into
// real history, polluting `git blame`/contributor stats on every harness-built
// PR (BEH-579). The values match the entrypoint's global identity so an
// in-container `git config user.name` read stays consistent.
const (
	HarnessAuthorName  = "Herd Agent Harness"
	HarnessAuthorEmail = "agent-harness@beherd.co"
)

// WorktreeReadySentinel is the readiness marker scripts/new-worktree.sh touches at
// a worktree's root on completion (BEH-549). It is gitignored on current main, but a
// feature branch based on a main that predates that .gitignore entry checks out a
// tree where it is NOT ignored — so it surfaces as an untracked `?? .worktree-ready`
// in `git status --porcelain`. The harness must treat it as a non-change regardless
// of .gitignore so it never blocks a push or rides into a recovery commit (BEH-612).
const WorktreeReadySentinel = ".worktree-ready"

// identityArgs are the `-c user.name=… -c user.email=…` overrides that stamp the
// harness bot identity on a host-side commit/rebase regardless of the ambient
// (possibly placeholder) repo config. A `-c` override beats both the local and
// global config levels, so it wins over the leaked `Test <test@example.com>`
// local config a host checkout may carry (BEH-579).
func identityArgs() []string {
	return []string{"-c", "user.name=" + HarnessAuthorName, "-c", "user.email=" + HarnessAuthorEmail}
}

// remoteOpTimeout bounds a single git remote attempt (fetch/push). A stalled
// remote — a black-hole network, a blocking credential prompt — would otherwise
// hang an attempt forever, defeating the retry loop entirely (BEH-386). It is
// per-attempt; the overall wall-clock cap is remoteRetryBudget plus at most one
// in-flight attempt of this length.
const remoteOpTimeout = 2 * time.Minute

func execRun(name string, args ...string) error {
	return proc.Run(remoteOpTimeout, name, args...)
}

// remoteRetryBudget caps the *total wall-clock* a git remote op (fetch/push) will
// spend retrying transient failures before giving up. BEH-329's window (3 attempts
// ~2s apart, ≈4s) was far too tight: a credential/network blip can last a minute
// or two — a momentary keychain lock, a brief network hiccup — surfacing as
// repeated `exit status 128`. In the BEH-403 post-mortem a fetch and a push failed
// ~2 min apart and *both* fell entirely inside the disruption, so every attempt
// failed and a green, gate-passed branch was left unpushed. A multi-minute budget
// rides out that class of event; it stays bounded so a genuine outage can't hang
// the pipeline (each attempt is independently capped by remoteOpTimeout). A var so
// tests can tune it.
//
// The budget covers *transient* failures only. Authentication/authorization
// rejections (401/403) are explicitly out of scope — they are permanent and can't
// clear on retry, so withRetry fails them fast via nonTransient rather than spend
// the whole budget stalled, which reads as a hang to the operator (BEH-475).
var remoteRetryBudget = 5 * time.Minute

// remoteBaseDelay and remoteMaxDelay frame the exponential backoff between
// attempts: start small so a sub-second flicker is ridden out cheaply, double each
// time, and cap so late retries stay reasonably responsive within the budget.
// Vars so tests can tune them.
var (
	remoteBaseDelay = 2 * time.Second
	remoteMaxDelay  = 30 * time.Second
)

// authFailureMarkers are stderr fragments git (and GitHub) emit when a remote op
// is rejected for *credentials*, not connectivity. proc.Run folds only git's *last*
// non-empty stderr line into the returned error (BEH-404), so a marker matches in
// production only when it lands on that final line. Over the HTTPS path the harness
// uses, every real denial does — `…returned error: 40[13]`, `fatal: Authentication
// failed`, `fatal: could not read Username`. The remaining markers (`write access…`,
// the bare RPC `40[13]` forms) sit on a non-final line in some shapes and are kept
// only as cheap belt-and-suspenders; don't count on them firing. Matched
// case-insensitively. Kept deliberately tight to auth/authz so a transient class
// (network, 5xx, lock) is never misclassified as permanent.
var authFailureMarkers = []string{
	"error: 401",                             // …returned error: 401
	"error: 403",                             // …returned error: 403
	"authentication failed",                  // fatal: Authentication failed for '…'
	"could not read username",                // fatal: could not read Username … prompts disabled
	"write access to repository not granted", // ERROR: Write access to repository not granted.
	"403 forbidden",
	"401 unauthorized",
}

// nonTransient reports whether err is a git failure that retrying cannot fix: an
// authentication/authorization rejection (BEH-475). A 401/403 means the token is
// wrong, expired, unauthorized, SSO-ungated, or lacks the repo grant — every retry
// fails identically, so spending the whole remoteRetryBudget on it is pure dead
// time that, worse, masquerades as a hang. Transient classes (network blips,
// remoteOpTimeout kills, 5xx, lock contention) are not matched and keep their retry
// budget (BEH-403).
func nonTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range authFailureMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// withRetry runs op repeatedly with exponential backoff until it succeeds or the
// wall-clock budget is spent, returning nil on success else the last error. It
// always makes at least one attempt, and never sleeps toward a deadline the next
// attempt couldn't beat. A non-transient failure (a 401/403 auth rejection — see
// nonTransient) short-circuits the budget: it can't be ridden out, so retrying it
// is dead time that looks like a hang (BEH-475). The clock (now) and sleeper are
// injected so the timing is exercised deterministically in tests with no real
// waiting.
func withRetry(op func() error, sleep func(time.Duration), now func() time.Time) error {
	deadline := now().Add(remoteRetryBudget)
	delay := remoteBaseDelay
	var err error
	for {
		if err = op(); err == nil {
			return nil
		}
		// A permanent auth failure can't clear on retry — return at once rather than
		// spend the whole budget stalled, which reads as a hang to the operator.
		if nonTransient(err) {
			return err
		}
		// Stop once the next backoff would carry us to/past the budget — no point
		// sleeping toward a deadline the following attempt couldn't beat.
		if !now().Add(delay).Before(deadline) {
			return err
		}
		sleep(delay)
		if delay < remoteMaxDelay {
			if delay *= 2; delay > remoteMaxDelay {
				delay = remoteMaxDelay
			}
		}
	}
}

// mainHistoryLookback bounds how far back the dispatch guard scans origin/main
// for a prior merge of the ticket. 50 commits is generous headroom over the
// finding's example (the merge was the 2nd commit back) while keeping the read
// trivial; work that merged further back than this is not something the harness
// is at risk of freshly re-dispatching.
const mainHistoryLookback = 50

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
func TicketAlreadyOnMain(herdPath, key string) bool {
	// Best-effort refresh; an offline/blipping remote just means we scan whatever
	// origin/main we already have rather than block dispatch behind the network.
	_ = execRun("git", "-C", herdPath, "fetch", "-q", "origin", "main")
	out, err := exec.Command(
		"git", "-C", herdPath, "log", "--oneline", "-"+strconv.Itoa(mainHistoryLookback), "origin/main",
	).Output()
	if err != nil {
		return false
	}
	return mainHistoryReferences(string(out), key)
}

// mainHistoryReferences reports whether `git log` output contains a commit
// referencing the exact ticket Key. The Key is tracker-agnostic (ADR-0010): the
// match quotes it literally and makes no `BEH-` assumption, so a Jira `PROJ-123`
// works identically. Matched on word boundaries so BEH-52 never matches BEH-521
// and BEH-521 never matches BEH-5210 (a substring grep — what the finding
// literally proposed — would conflate those), and case-insensitively because a
// subject sometimes lower-cases the key.
func mainHistoryReferences(logOutput, key string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(key) + `\b`).MatchString(logOutput)
}

// WorktreePath is the host path of the worktree the tdd skill is told to create.
func WorktreePath(herdPath, slug string) string {
	return filepath.Join(herdPath, ".claude", "worktrees", slug)
}

// BranchName is the deterministic feature branch the tdd skill creates for a slug.
func BranchName(slug string) string {
	return "feat/" + slug
}

// HeadSHA returns the commit SHA at the worktree's HEAD, read host-side via the
// real-path mount (the same seam WorktreeClean/CheckpointCommit use). It lets a
// caller capture the tip before a session and tell afterwards whether that
// session actually committed anything.
func HeadSHA(worktreePath string) (string, error) {
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// FetchMain fast-forwards the primary checkout's view of origin/main so commit
// ranges and the PR base are current (DESIGN.md: pull origin/main after every
// session). Retried against transient remote blips; a fetch failure is returned
// for the caller to log, not fatal.
func FetchMain(herdPath string) error {
	return fetchMain(herdPath, execRun, time.Sleep, time.Now)
}

func fetchMain(herdPath string, run commandRunner, sleep func(time.Duration), now func() time.Time) error {
	return withRetry(func() error {
		return run("git", "-C", herdPath, "fetch", "-q", "origin", "main")
	}, sleep, now)
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
	return !porcelainHasRealChanges(string(out))
}

// porcelainHasRealChanges reports whether `git status --porcelain` output names any
// change other than the readiness sentinel (BEH-612). Porcelain v1 lines are two
// status columns + a space + the path; the sentinel never has a special-char path,
// so it is never quoted or rename-formatted.
func porcelainHasRealChanges(porcelain string) bool {
	for _, line := range strings.Split(porcelain, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		path := line
		if len(line) > 3 {
			path = line[3:]
		}
		if strings.TrimSpace(path) == WorktreeReadySentinel {
			continue
		}
		return true
	}
	return false
}

// RebaseResult classifies a rebase-of-the-feature-branch-onto-origin/main attempt.
type RebaseResult int

const (
	// RebaseClean — the branch replayed onto origin/main with no conflicts (or was
	// already current). Its tip may now carry rewritten SHAs, so a branch already on
	// the remote needs a force-with-lease re-push (PushForceWithLease); a not-yet-
	// pushed branch ships with a plain Push.
	RebaseClean RebaseResult = iota
	// RebaseConflict — the rebase could not be applied cleanly (a genuine content
	// conflict, or any other failure) and was aborted, restoring the branch exactly
	// as it was. Resolution is left to a human (BEH-570).
	RebaseConflict
)

// rebaseBackupRef marks the feature tip across the reset → cherry-pick replay
// (BEH-618). `reset --hard origin/main` discards the feature commits from the branch
// ref (they'd survive only in the reflog), so we pin them on a throwaway ref the
// cherry-pick range can name and the conflict path can restore from. It lives under
// refs/harness/ so it never shows in `git branch` and a crashed prior run's stale ref
// is harmlessly overwritten.
const rebaseBackupRef = "refs/harness/rebase-onto-main"

// RebaseOntoMain replays the worktree's feature branch onto origin/main so the PR
// opens on a current base instead of the stale one a long pipeline accreted (a
// sibling PR merging underneath it — BEH-570). origin/main must already be fresh
// (call FetchMain first) and the worktree clean. Run host-side against the worktree,
// whose absolute .git resolves via the real-path mount (ADR-0002) — the same seam
// WorktreeClean/CheckpointCommit use.
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
func RebaseOntoMain(worktreePath string) RebaseResult {
	return rebaseOntoMain(worktreePath, execRun)
}

func rebaseOntoMain(worktreePath string, run commandRunner) RebaseResult {
	// Strip the readiness sentinel before replaying. new-worktree.sh drops an untracked
	// .worktree-ready into every worktree (BEH-549), and a checkout that wants to write a
	// now-tracked .worktree-ready over the untracked copy can refuse ("untracked working
	// tree files would be overwritten") — the BEH-617 trap. Best-effort: a no-op (os
	// error) when the sentinel is absent.
	_ = os.Remove(filepath.Join(worktreePath, WorktreeReadySentinel))

	// Pin the feature tip so `reset --hard` can't lose the commits and the cherry-pick
	// range / conflict-restore can name them.
	if run("git", "-C", worktreePath, "update-ref", rebaseBackupRef, "HEAD") != nil {
		return RebaseConflict
	}
	defer func() { _ = run("git", "-C", worktreePath, "update-ref", "-d", rebaseBackupRef) }()

	// Move the branch onto the fresh base. reset --hard escapes the rebase checkout
	// false-fail and also fast-forwards a branch that is merely behind origin/main.
	if run("git", "-C", worktreePath, "reset", "--hard", "origin/main") != nil {
		return RebaseConflict
	}

	// Nothing to replay if the feature tip is already contained in origin/main (the
	// branch was behind or equal): the reset alone completed the move. cherry-pick of
	// an empty range errors, so short-circuit it.
	if run("git", "-C", worktreePath, "merge-base", "--is-ancestor", rebaseBackupRef, "origin/main") == nil {
		return RebaseClean
	}

	// Replay the feature's own commits (origin/main..tip) onto the new base. cherry-pick
	// rewrites the COMMITTER of every replayed commit to whoever runs it, so stamp the
	// harness identity — exactly as the old rebase did — so the pushed branch never
	// inherits the host checkout's placeholder identity (BEH-579).
	//
	// --empty=drop makes a commit whose diff is already present identically in
	// origin/main (a sibling PR merged the same change, or a hotfix was cherry-picked
	// to main) auto-drop and the replay continue, instead of halting with "the previous
	// cherry-pick is now empty" (exit 1) — which a bare cherry-pick does and which we'd
	// misread as a genuine conflict, needlessly burning a sandboxed resolution session.
	// This matches an ideal `git rebase`'s auto-drop (BEH-622). Requires git ≥ 2.45
	// (May 2024); the harness runs host-side (see README prerequisites).
	args := append([]string{"-C", worktreePath}, identityArgs()...)
	args = append(args, "cherry-pick", "--empty=drop", "origin/main.."+rebaseBackupRef)
	if run("git", args...) != nil {
		// Genuine content conflict: abort the cherry-pick and restore the branch to its
		// original tip, leaving it exactly as it was for a human (BEH-570). Both are
		// best-effort — if the cherry-pick never started, --abort fails harmlessly.
		_ = run("git", "-C", worktreePath, "cherry-pick", "--abort")
		_ = run("git", "-C", worktreePath, "reset", "--hard", rebaseBackupRef)
		return RebaseConflict
	}
	return RebaseClean
}

// IsRebasedOnto reports whether ref (e.g. "origin/main") is an ancestor of the
// worktree's HEAD — i.e. the branch genuinely contains the latest base. It is the
// ground-truth guard the pre-push conflict-resolution path needs (BEH-581): a
// sandboxed session that gives up and runs `git rebase --abort` leaves a CLEAN
// worktree on the original stale tip, so WorktreeClean alone would wave it
// through. Confirming the rebase actually landed stops the harness re-gating and
// pushing a still-stale branch. Read host-side via the real-path mount; any git
// failure (the non-ancestor exit, or git refusing) reads as not-rebased.
func IsRebasedOnto(worktreePath, ref string) bool {
	return isRebasedOnto(worktreePath, ref, execRun)
}

func isRebasedOnto(worktreePath, ref string, run commandRunner) bool {
	return run("git", "-C", worktreePath, "merge-base", "--is-ancestor", ref, "HEAD") == nil
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
func IsDisjointFrom(worktreePath, ref string) bool {
	return isDisjointFrom(worktreePath, ref, execRun)
}

func isDisjointFrom(worktreePath, ref string, run commandRunner) bool {
	return branchesDisjoint(worktreePath, ref, "HEAD", run)
}

// branchesDisjoint reports whether refA and refB share no common ancestor — `git
// merge-base refA refB` exits non-zero with empty output for a disjoint history.
// The shared core behind IsDisjointFrom (worktree HEAD vs a ref) and the tdd
// ground-truth gather (feat/<slug> vs origin/main, read from the main checkout).
func branchesDisjoint(dir, refA, refB string, run commandRunner) bool {
	return run("git", "-C", dir, "merge-base", refA, refB) != nil
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
func RegraftOntoBase(worktreePath, ref string) error {
	// Capture the content diff and handoff message BEFORE moving the branch.
	// --binary so binary blobs / mode / deletion changes regraft faithfully.
	patch, err := exec.Command("git", "-C", worktreePath, "diff", "--binary", ref, "HEAD").Output()
	if err != nil {
		return fmt.Errorf("capturing content diff against %s: %w", ref, err)
	}
	if len(bytes.TrimSpace(patch)) == 0 {
		return fmt.Errorf("no content diff against %s — nothing to regraft", ref)
	}
	msg, err := exec.Command("git", "-C", worktreePath, "log", "-1", "--format=%B").Output()
	if err != nil {
		return fmt.Errorf("reading handoff message: %w", err)
	}
	// Re-root: reset --hard moves the branch ref onto ref's tip (escaping the
	// disjoint root; the old tip survives in the reflog) and matches the worktree.
	if err := execRun("git", "-C", worktreePath, "reset", "--hard", ref); err != nil {
		return fmt.Errorf("resetting onto %s: %w", ref, err)
	}
	// Re-apply the captured diff onto the fresh base (index + worktree).
	apply := exec.Command("git", "-C", worktreePath, "apply", "--index")
	apply.Stdin = bytes.NewReader(patch)
	if out, aerr := apply.CombinedOutput(); aerr != nil {
		return fmt.Errorf("re-applying diff onto %s: %w (%s)", ref, aerr, strings.TrimSpace(string(out)))
	}
	// Commit the regrafted tree, preserving the handoff message with a regraft note.
	commitMsg := strings.TrimRight(string(msg), "\n") +
		"\n\nRe-grafted onto " + ref + " to escape a disjoint history (BEH-609)."
	args := append([]string{"-C", worktreePath}, identityArgs()...)
	args = append(args, "commit", "--no-verify", "-m", commitMsg)
	if err := execRun("git", args...); err != nil {
		return fmt.Errorf("committing regrafted diff: %w", err)
	}
	return nil
}

// BranchDiffEmpty reports whether the worktree's committed tip makes ZERO net
// change against origin/main (an empty `git diff origin/main`) — the empty-commit
// branch the harness wrongly opened as PR #642 (BEH-603). It is the detection half
// of the recommend-close disposition: a clean, gate-green, reviewed branch with an
// empty diff has nothing to ship, so the ticket should be closed as a
// duplicate/superseded rather than opened as an empty-commit PR. Callers must check
// WorktreeClean first — `git diff origin/main` includes uncommitted changes, so on a
// dirty tree an "empty" committed diff could still hide real uncommitted work.
//
// `git diff --quiet` exits 0 when there is no diff and non-zero when there is, so
// only a clean exit-0 reports empty. Any error (a non-zero diff exit, or an
// unresolvable ref) reads as NOT empty — the fail-safe direction: the harness would
// rather attempt the push than wrongly recommend closing a ticket on doubt. Read
// host-side via the real-path mount, the same seam WorktreeClean/RebaseOntoMain use.
func BranchDiffEmpty(worktreePath string) bool {
	return branchDiffEmpty(worktreePath, execRun)
}

func branchDiffEmpty(worktreePath string, run commandRunner) bool {
	return run("git", "-C", worktreePath, "diff", "--quiet", "origin/main") == nil
}

// AbortRebase restores a worktree a conflict-resolution session left mid-replay to a
// clean, on-branch state before the harness keeps it for a human (BEH-581). The
// session replays via `cherry-pick` (BEH-618), so a give-up can strand a cherry-pick
// in progress that `rebase --abort` won't clean — abort BOTH. Best-effort: if neither
// is in progress the aborts fail harmlessly, so there is nothing to surface — callers
// fire it unconditionally on a failed resolution.
func AbortRebase(worktreePath string) {
	abortRebase(worktreePath, execRun)
}

func abortRebase(worktreePath string, run commandRunner) {
	_ = run("git", "-C", worktreePath, "rebase", "--abort")
	_ = run("git", "-C", worktreePath, "cherry-pick", "--abort")
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
// remote blips under a wall-clock budget (BEH-329, widened in BEH-403).
func Push(herdPath, slug string) error {
	return push(herdPath, slug, execRun, time.Sleep, time.Now)
}

func push(herdPath, slug string, run commandRunner, sleep func(time.Duration), now func() time.Time) error {
	return withRetry(func() error {
		return run("git", "-C", herdPath, "push", "--no-verify", "origin", BranchName(slug))
	}, sleep, now)
}

// PushForceWithLease re-pushes the feature branch after an auto-rebase rewrote its
// history (BEH-570). It is needed only on the reactive path — a branch already on
// the remote whose tip the rebase moved — so a plain Push would be rejected as
// non-fast-forward. --force-with-lease is the safe force: it refuses to overwrite
// remote commits the harness hasn't observed (it never will here — the harness owns
// the branch — but the lease is the correct, non-destructive force). --no-verify and
// the transient-retry budget match Push.
func PushForceWithLease(herdPath, slug string) error {
	return pushForceWithLease(herdPath, slug, execRun, time.Sleep, time.Now)
}

func pushForceWithLease(herdPath, slug string, run commandRunner, sleep func(time.Duration), now func() time.Time) error {
	return withRetry(func() error {
		return run("git", "-C", herdPath, "push", "--no-verify", "--force-with-lease", "origin", BranchName(slug))
	}, sleep, now)
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

	// Disjoint history: feat/<slug> shares no common ancestor with origin/main (an
	// empty merge-base — the BEH-355 condition). Only checked when the branch is
	// ahead, so both refs resolve and a non-zero merge-base means genuine disjoint
	// rather than an unresolved ref; an absent/zero-ahead branch already fails the
	// CommitsAhead gate (BEH-597).
	disjoint := commitsAhead > 0 && branchesDisjoint(herdPath, "feat/"+slug, "origin/main", execRun)

	return verify.GroundTruth{WorktreeExists: worktreeExists, CommitsAhead: commitsAhead, DisjointHistory: disjoint}
}

// BranchExists reports whether `feat/<slug>` resolves to a git revision in the
// main checkout — i.e. the upstream /tdd session actually created the feature
// branch. It reads the LOCAL head ref (`refs/heads/feat/<slug>`), where the
// sandbox's commits land via the shared `.git`; distinct from BranchPushed,
// which reads the remote-tracking ref. It is the retrospective's host-side
// precondition (BEH-553): a branch that doesn't resolve means there is no diff to
// retrospect. Any git failure → false (treat an unreadable ref as absent).
func BranchExists(herdPath, slug string) bool {
	err := exec.Command(
		"git", "-C", herdPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+BranchName(slug),
	).Run()
	return err == nil
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

// StripWorktreeNodeModules removes `web/node_modules` from the worktree the
// implementation tool hands back. The sandbox builds the worktree on linux-arm64
// (ADR-0002), so that tree carries Linux-only native bindings (`@oxlint/...`,
// `@oxfmt/...`, `@rolldown/...`); a reviewer running the gates on a non-Linux host
// hits a cryptic `MODULE_NOT_FOUND` and `pnpm install --frozen-lockfile` won't
// repair it (the platform-conditional optional deps look satisfied). Stripping the
// tree means the reviewer always installs fresh for their own platform — the
// review-worktree skill already treats a missing `node_modules` as "run install
// first" (BEH-412). Idempotent: a no-op when the dir is already absent.
func StripWorktreeNodeModules(worktreePath string) error {
	return os.RemoveAll(filepath.Join(worktreePath, "web", "node_modules"))
}

// CheckpointCommit captures whatever uncommitted work a finished session left in
// the worktree as a recovery commit on the feature branch, so a session that
// ended (wall-clock cap / usage-policy refusal / crash) before committing its work
// leaves a recoverable commit instead of a bare worktree that needs manual rescue.
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
// via the real-path mount (ADR-0002), the same seam WorktreeClean uses.
func CheckpointCommit(worktreePath, identifier, session string) error {
	if WorktreeClean(worktreePath) {
		return nil
	}
	return checkpointCommit(worktreePath, CheckpointMessage(identifier, session), execRun)
}

func checkpointCommit(worktreePath, message string, run commandRunner) error {
	if err := run("git", "-C", worktreePath, "add", "-A"); err != nil {
		return err
	}
	// Never let the readiness sentinel (BEH-549) ride into a recovery commit: on a
	// stale-base branch where it is not yet gitignored, `add -A` stages it. Unstaging
	// it keeps the checkpoint to real session work and stops the gitignored artifact
	// reattaching as a tracked file (BEH-612). Best-effort — a no-op when absent.
	_ = run("git", "-C", worktreePath, "reset", "-q", "--", WorktreeReadySentinel)
	// Belt-and-braces: refuse to commit when nothing real is staged. `git diff
	// --cached --quiet` exits 0 (nil) iff the index matches HEAD — so a checkpoint
	// can never capture zero work (e.g. a sentinel-only tree the guard let through).
	if run("git", "-C", worktreePath, "diff", "--cached", "--quiet") == nil {
		return nil
	}
	args := append([]string{"-C", worktreePath}, identityArgs()...)
	args = append(args, "commit", "--no-verify", "-m", message)
	return run("git", args...)
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
func EnsureCIRerunCommit(worktreePath, headBefore string) error {
	return ensureCIRerunCommit(worktreePath, headBefore, execRun)
}

func ensureCIRerunCommit(worktreePath, headBefore string, run commandRunner) error {
	head, err := HeadSHA(worktreePath)
	if err != nil {
		return err
	}
	// HEAD moved → the agent committed a real fix; that is what re-triggers CI.
	if head != headBefore {
		return nil
	}
	args := append([]string{"-C", worktreePath}, identityArgs()...)
	args = append(args, "commit", "--no-verify", "--allow-empty", "-m", CIRerunMessage())
	return run("git", args...)
}

// CIRerunMessage is the subject+body for the harness's empty re-trigger commit.
// It loudly marks the commit as a no-code-change CI re-run so a reviewer reading
// PR history sees why an empty commit exists rather than mistaking it for a fix.
func CIRerunMessage() string {
	return "chore(harness): re-trigger CI (no code change)\n\n" +
		"The auto-fix session reproduced the gates locally and found no code defect:\n" +
		"the CI red was a cancelled/superseded/flaky run, not a reproducible failure.\n" +
		"This empty commit gives CI a fresh HEAD to re-run against instead of a\n" +
		"fabricated, speculative change."
}

// CheckpointMessage builds the commit message for a harness recovery checkpoint.
// The subject is loudly prefixed and names the `session` (tdd / review) so a
// reviewer — and a resumed review re-deriving the diff — can tell a salvaged-on-
// timeout diff apart from a real, verified handoff, and know which stage left it.
func CheckpointMessage(identifier, session string) string {
	return "checkpoint(harness): recover uncommitted " + session + " session work (" + identifier + ")\n\n" +
		"Harness-created safety net: the " + session + " session left this diff\n" +
		"uncommitted in the worktree (wall-clock cap, usage-policy refusal, or crash)\n" +
		"before committing its work. This is NOT a verified handoff — finish the work\n" +
		"or re-run, then squash/amend, before opening a PR."
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
