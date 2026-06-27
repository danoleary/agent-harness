// Package git gathers the ground truth a finished tdd session leaves behind,
// read from the host's primary checkout.
package git

import (
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

// TicketAlreadyOnMain reports whether the ticket key already appears in recent
// origin/main history — i.e. its work merged, so dispatching a fresh tdd session
// would burn a whole worktree + install only to discover an empty diff and raise
// no PR (BEH-528). It first refreshes origin/main with a single best-effort fetch
// (the host checkout's remote-tracking ref can lag a just-merged PR), then scans.
//
// It fails OPEN: any git error (no remote, detached/corrupt checkout, a fetch
// blip) returns false so a flaky read never blocks a legitimate dispatch. The
// asymmetry is deliberate — a false negative costs one session (the pre-guard
// status quo), whereas a false positive would silently drop real work.
func TicketAlreadyOnMain(herdPath, identifier string) bool {
	// Best-effort refresh; an offline/blipping remote just means we scan whatever
	// origin/main we already have rather than block dispatch behind the network.
	_ = execRun("git", "-C", herdPath, "fetch", "-q", "origin", "main")
	out, err := exec.Command(
		"git", "-C", herdPath, "log", "--oneline", "-"+strconv.Itoa(mainHistoryLookback), "origin/main",
	).Output()
	if err != nil {
		return false
	}
	return mainHistoryReferences(string(out), identifier)
}

// mainHistoryReferences reports whether `git log` output contains a commit
// referencing the exact ticket key. Matched on word boundaries so BEH-52 never
// matches BEH-521 and BEH-521 never matches BEH-5210 (a substring grep — what
// the finding literally proposed — would conflate those), and case-insensitively
// because a subject sometimes lower-cases the key.
func mainHistoryReferences(logOutput, identifier string) bool {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(identifier) + `\b`).MatchString(logOutput)
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
// remote blips under a wall-clock budget (BEH-329, widened in BEH-403).
func Push(herdPath, slug string) error {
	return push(herdPath, slug, execRun, time.Sleep, time.Now)
}

func push(herdPath, slug string, run commandRunner, sleep func(time.Duration), now func() time.Time) error {
	return withRetry(func() error {
		return run("git", "-C", herdPath, "push", "--no-verify", "origin", BranchName(slug))
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

	return verify.GroundTruth{WorktreeExists: worktreeExists, CommitsAhead: commitsAhead}
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

// CheckpointCommit captures whatever uncommitted work a finished tdd session left
// in the worktree as a recovery commit on the feature branch, so a session that
// ended (wall-clock cap / usage-policy refusal / crash) before reaching its own
// handoff commit leaves a recoverable commit instead of a bare worktree that needs
// manual rescue (BEH-479: the cap fired during a final verification re-run and
// discarded a finished diff). It is a SAFETY NET, not a verdict — the work is
// unverified, so the commit subject loudly marks it a harness checkpoint. Staging
// is `-A` (this is recovery: capture every change, tracked and untracked) and the
// commit is `--no-verify` (the work may not pass hooks — that is precisely why it
// is a checkpoint and not a handoff). A no-op success when the worktree is already
// clean (nothing was left behind to recover). Run host-side against the worktree
// via the real-path mount (ADR-0002), the same seam WorktreeClean uses.
func CheckpointCommit(worktreePath, identifier string) error {
	if WorktreeClean(worktreePath) {
		return nil
	}
	return checkpointCommit(worktreePath, CheckpointMessage(identifier), execRun)
}

func checkpointCommit(worktreePath, message string, run commandRunner) error {
	if err := run("git", "-C", worktreePath, "add", "-A"); err != nil {
		return err
	}
	return run("git", "-C", worktreePath, "commit", "--no-verify", "-m", message)
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
	return run("git", "-C", worktreePath, "commit", "--no-verify", "--allow-empty", "-m", CIRerunMessage())
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
// The subject is loudly prefixed so a reviewer (and a future verify step or
// recovery script) can tell a salvaged-on-timeout diff apart from a real,
// verified tdd handoff commit.
func CheckpointMessage(identifier string) string {
	return "checkpoint(harness): recover uncommitted session work (" + identifier + ")\n\n" +
		"Harness-created safety net: the tdd session left this diff uncommitted in\n" +
		"the worktree (wall-clock cap, usage-policy refusal, or crash) before it\n" +
		"reached its own handoff commit. This is NOT a verified handoff — finish the\n" +
		"work or re-run, then squash/amend, before opening a PR."
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
