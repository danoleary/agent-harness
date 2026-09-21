// Package git is the harness's host-side git: one [Checkout] (the Consumer's
// primary checkout) that mints one [Worktree] per ticket, and every operation the
// harness performs on either as a method on those two types.
//
// Before this package had types it had thirty free functions, and "the worktree
// for ticket X" — the harness's central noun — was three strings
// (herdPath, branchPrefix, slug) re-derived at every call site. Twelve of those
// functions came in exported/unexported twin pairs, the lowercase half taking an
// injected runner so a test could reach it; the exported half, which is what the
// harness actually calls, was the untested one. The mutating remote operations —
// Push, PushForceWithLease, CreateWorktree — were on the untested side, and with
// them the entire transient-failure retry ([withRetry], [nonTransient], the
// budget) that decides whether a green, gate-passed branch reaches origin.
//
// The seam is now a single [runners] value the two types carry: every side effect
// in the package goes through one of its five fields, and a test constructs a
// Checkout over fakes and drives the real exported methods.
package git

import (
	"bytes"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/proc"
)

// commandRunner runs a command to completion, returning only its error.
// Production uses execRun; tests inject a fake to drive the retry logic
// without touching a real remote (mirrors sandbox.Preflight's runner seam).
type commandRunner func(name string, args ...string) error

// outputRunner runs a command and returns its stdout + error — the output-capturing
// sibling of commandRunner, so a reading operation (git log, git status, git diff
// --name-only) is unit-testable with an injected fake. Production uses execOutput.
type outputRunner func(name string, args ...string) ([]byte, error)

// pipeRunner runs a command with stdin fed from a byte slice, returning its
// combined output — the one shape the other two can't express, needed by
// [Worktree.RegraftOntoBase]'s `git apply --index`. Production uses execPipe.
type pipeRunner func(stdin []byte, name string, args ...string) ([]byte, error)

// runners is the package's single injection point: the three command shapes plus
// the clock the remote-retry budget is measured against. [Checkout] and
// [Worktree] carry one by value, so replacing it replaces the seam for every
// operation either type performs — one hook where there were twelve twin pairs.
type runners struct {
	run    commandRunner
	output outputRunner
	pipe   pipeRunner
	sleep  func(time.Duration)
	now    func() time.Time
}

// execRunners is the production seam: the real git binary, the real clock.
func execRunners() runners {
	return runners{run: execRun, output: execOutput, pipe: execPipe, sleep: time.Sleep, now: time.Now}
}

func execOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func execPipe(stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	return cmd.CombinedOutput()
}

// HarnessAuthorName / HarnessAuthorEmail are the dedicated bot identity the
// harness stamps on every commit it is responsible for: the host-side recovery
// ([Worktree.Checkpoint]) and CI-rerun ([Worktree.EnsureCIRerunCommit]) commits it
// makes directly, the committer of the commits it rebases before pushing
// ([Worktree.Rebase]), and — via sandbox.BuildDockerRunArgs' GIT_AUTHOR_*/
// GIT_COMMITTER_* env — the agent's in-container handoff commit. Without it the
// sandbox checkout's placeholder `Test <test@example.com>` LOCAL git config —
// which overrides the entrypoint's `git config --global` identity — leaks into
// real history, polluting `git blame`/contributor stats on every harness-built
// PR (BEH-579). The values match the entrypoint's global identity so an
// in-container `git config user.name` read stays consistent.
const (
	HarnessAuthorName  = "Agent Harness"
	HarnessAuthorEmail = "agent-harness@users.noreply.github.com"
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
// [nonTransient]) short-circuits the budget: it can't be ridden out, so retrying it
// is dead time that looks like a hang (BEH-475). The clock and sleeper come from
// the [runners] seam, so the timing is exercised deterministically in tests with no
// real waiting — and, unlike the twin-pair arrangement this replaces, through the
// same exported methods production calls.
func (r runners) withRetry(op func() error) error {
	deadline := r.now().Add(remoteRetryBudget)
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
		if !r.now().Add(delay).Before(deadline) {
			return err
		}
		r.sleep(delay)
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

// mainHistoryReferences reports whether `git log` output contains a commit
// referencing the exact ticket Key. The Key is tracker-agnostic (ADR-0010): the
// match quotes it literally and makes no `BEH-` assumption, so a Jira `PROJ-123`
// works identically. Matched on word boundaries so BEH-52 never matches BEH-521
// and BEH-521 never matches BEH-5210 (a substring grep — what the finding
// literally proposed — would conflate those), and case-insensitively because a
// subject sometimes lower-cases the key.
func mainHistoryReferences(logOutput, key string) bool {
	return keyReferenced(logOutput, key)
}

// keyReferenced reports whether text word-boundary-matches the ticket Key,
// case-insensitively. Shared by the main-history scan and the remote-branch scan so
// both apply identical, Key-agnostic word-boundary semantics (BEH-52 ≠ BEH-521).
func keyReferenced(text, key string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(key) + `\b`).MatchString(text)
}

// remoteBranchesReference reports whether `git ls-remote --heads` output contains a
// branch name referencing the exact ticket Key, on word boundaries so a shorter key
// is never a prefix-match of a longer branch's key.
func remoteBranchesReference(lsRemoteOutput, key string) bool {
	return keyReferenced(lsRemoteOutput, key)
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
	// the remote needs a force-with-lease re-push ([Worktree.PushForceWithLease]); a
	// not-yet-pushed branch ships with a plain [Worktree.Push].
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

// maxCherryPickSkips bounds the initially-empty auto-skip loop as a pure safety
// backstop (BEH-678). Each `cherry-pick --skip` consumes one commit from the replay
// todo, so a replay needs at most (commits-ahead) skips — a handful for any harness
// handoff. The cap only trips in a pathological git state that stops making progress,
// where fail-safe is to abort and hand the branch to a human rather than spin.
const maxCherryPickSkips = 100

// branchesDisjoint reports whether refA and refB share no common ancestor — `git
// merge-base refA refB` exits non-zero with empty output for a disjoint history.
// The shared core behind [Worktree.IsDisjointFrom] (worktree HEAD vs a ref) and the
// ground-truth gather (the feature branch vs origin/main, read from the main
// checkout).
func branchesDisjoint(run commandRunner, dir, refA, refB string) bool {
	return run("git", "-C", dir, "merge-base", refA, refB) != nil
}

// DocsOnlyPaths reports whether EVERY given repo-relative changed path is
// documentation/prose that no build gate or CI job reads — the condition under
// which the review host-gate re-run and the CI poll can be safely skipped for a
// change that provably cannot break the build (BEH-687). It is a conservative
// allowlist: any path it does not positively recognise as inert (source under a
// module tree, scripts, workflows, migrations, package manifests, …) makes the
// whole set non-docs-only, so the short-circuit never fires on a change that could
// affect a gate. An empty set is NOT docs-only — a branch with nothing to ship is
// the zero-net-diff case ([Worktree.DiffEmpty] / BEH-602), handled separately.
//
// excludedRoots comes from the Consumer's `docs_only_excluded_roots` config: the
// directories whose contents can feed a gate whatever they look like. It cannot
// be a harness constant, because the answer is a fact about the Consumer's own
// layout — `web/` and `supabase/` mean nothing to a .NET project, and a harness
// that guessed would classify that project's `src/Foo/README.md` as inert and
// skip a CI watch that its `src/**` trigger actually runs.
//
// An EMPTY list therefore disables the short-circuit rather than enabling it for
// everything: excluding more roots can only make the classifier more conservative
// (run a gate that would have run anyway), while excluding fewer risks the one
// failure that matters — silently merging past a job that could go red. A
// Consumer opts in by declaring its roots.
func DocsOnlyPaths(paths []string, excludedRoots []string) bool {
	if len(paths) == 0 || len(excludedRoots) == 0 {
		return false
	}
	for _, p := range paths {
		if !docsOnlyPath(p, excludedRoots) {
			return false
		}
	}
	return true
}

// docsOnlyPath reports whether one repo-relative changed path is inert prose. A
// path inside a module's source tree (web/, agent-harness/) is never inert even
// when it looks like prose — a README fixture or a Storybook doc can feed a gate —
// so it is excluded before the markdown/docs allowlist is consulted. The skill
// trees (.agents/, and its .claude/ symlink) are excluded for the same reason:
// their SKILL.md files are NOT inert, they are read by the agent-harness
// internal/skills contract tests, so a skill markdown edit triggers the Agent
// Harness CI job (agent-harness.yaml keys on `.agents/skills/**`) and can turn it
// red — treating it as docs-only would short-circuit a watch that could fail.
//
// A Consumer's excluded roots must stay a superset of every directory a workflow's
// `on.pull_request.paths` triggers on, so a markdown edit UNDER such a root
// (scripts/README.md, .github/workflows/notes.md) is never treated as inert while
// the workflow it triggers goes unwatched. A Consumer is expected to cross-check
// its declared roots against its own workflow triggers. Outside those trees,
// markdown anywhere and the repo-root docs/ tree feed no gate or CI job.
func docsOnlyPath(p string, excludedRoots []string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	for _, codeRoot := range excludedRoots {
		if strings.HasPrefix(p, codeRoot) {
			return false
		}
	}
	return strings.HasSuffix(p, ".md") || strings.HasPrefix(p, "docs/")
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
