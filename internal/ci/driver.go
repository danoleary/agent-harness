package ci

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/proc"
)

// errNoChecksYet marks the transient window right after a PR opens when Actions
// has not registered any checks: `gh pr checks` exits non-zero with "no checks
// reported" and no JSON. The poller treats it as pending (keep waiting), not as
// the hard failure a real gh error (auth, bad branch) would be.
var errNoChecksYet = errors.New("no CI checks reported yet")

// errChecksUnobservable marks the case where the GH_TOKEN can fetch/push/open the
// PR but cannot read the Checks API — a fine-grained PAT has no "Checks"
// permission, so `gh pr checks` 403s with "Resource not accessible by personal
// access token". This is a permission ceiling, not a real CI failure: the watch
// must degrade (leave the open PR for a human) rather than abort or auto-fix.
// Only a classic `repo`-scoped PAT can read check runs (BEH-476).
var errChecksUnobservable = errors.New("CI checks not observable with this token (needs a classic repo-scoped PAT)")

// isUnobservableErr reports whether gh's stderr carries GitHub's permission-denied
// signature for the Checks API. GitHub returns "Resource not accessible by …" for
// a token lacking the permission — distinct from "authentication required" / "Bad
// credentials" (a missing/invalid token) or "no checks reported" (transient).
func isUnobservableErr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "resource not accessible")
}

// checksJSONFields is the `gh pr checks --json` field set the driver reads: the
// name + coarse bucket to classify, plus state/link for diagnostics and run-id
// extraction.
const checksJSONFields = "name,bucket,state,link"

// defaultLogTailBytes bounds the failed-job logs injected into the fix prompt.
// CI logs can be enormous; the failure is at the tail, so the driver keeps the
// last chunk and drops the rest with a marker.
const defaultLogTailBytes = 16000

// Marker prefixes that fetchFailedLogs / truncateLogs emit as the *only* content
// when no real step output was retrievable (run cancelled/superseded/expired, no
// Actions runs at all, or a fetch error folded into the text). prompt.logsUnfetchable
// keys off these to detect an unfetchable payload and branch the CI-fix prompt
// framing. They are exported as the single source of truth so a marker rename
// here moves the producer and the predicate together — the two sides used to be
// coupled only by "keep in sync" comments, which let a marker edit silently revert
// the BEH-560 fix (BEH-563).
const (
	// RunHeaderMarker begins each run's section: "===== run <id> (failed steps) =====".
	RunHeaderMarker = "===== run "
	// FetchErrorMarker begins the folded-in gh error when a run's logs could not
	// be retrieved: "(could not fully fetch logs for run <id>: <err>)".
	FetchErrorMarker = "(could not fully fetch logs for run "
	// NoRunsSentinel is the entire payload when there were no Actions run ids.
	NoRunsSentinel = "(no GitHub Actions run logs available for the failing checks)"
	// TruncationHeadMarker begins truncateLogs's dropped-head notice; TruncationWord
	// also appears within it: "[… <n> earlier bytes truncated …]".
	TruncationHeadMarker = "[…"
	TruncationWord       = "truncated"
)

// GhDriver is the production Driver. Host-side `gh` (through internal/proc, per
// ADR-0002) does the poll / re-run / log-fetch; the sandbox fix and the git push
// are injected as callbacks so this package needn't import internal/session or
// internal/git (the cmd wires those, where the rest of the harness plumbing
// lives).
type GhDriver struct {
	herdPath     string
	branch       string // feat/<slug> — the PR head gh keys checks off
	pollCfg      pollConfig
	ghTimeout    time.Duration
	logTailBytes int

	// runFix launches the sandboxed Claude session over the worktree to diagnose
	// + fix + commit, given the failing CI logs. Returns non-nil on session failure.
	runFix func(ciLogs string) error
	// push pushes the new fix commit to the PR branch (gitpkg.Push).
	push func() error

	sleep func(time.Duration)
	now   func() time.Time
}

// NewGhDriver builds the production Driver. ghTimeout bounds each individual gh
// call; cfg supplies the poll cadence/budget; runFix and push are the sandbox +
// remote effects the cmd provides.
func NewGhDriver(herdPath, branch string, cfg Config, ghTimeout time.Duration, runFix func(ciLogs string) error, push func() error) *GhDriver {
	return &GhDriver{
		herdPath:     herdPath,
		branch:       branch,
		pollCfg:      pollConfig{interval: cfg.PollInterval, budget: cfg.PollBudget},
		ghTimeout:    ghTimeout,
		logTailBytes: defaultLogTailBytes,
		runFix:       runFix,
		push:         push,
		sleep:        time.Sleep,
		now:          time.Now,
	}
}

// Poll runs `gh pr checks <branch> --json …` until the checks reach a terminal
// verdict or the poll budget is spent.
func (d *GhDriver) Poll() (Verdict, []Check, error) {
	fetch := func() ([]Check, error) {
		stdout, stderr, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "gh", "pr", "checks", d.branch, "--json", checksJSONFields)
		return interpretChecksOutput(stdout, stderr, err)
	}
	return poll(fetch, d.pollCfg, d.sleep, d.now)
}

// Rerun re-triggers the failed jobs of each backing Actions run (the flake wash),
// then waits one poll interval before returning so GitHub can flip those checks
// back to pending. Without the wait, WatchAndFix's immediate re-poll can observe
// the *stale* failed bucket — the rerun has not registered yet — and mistake a
// flake for a real failure, jumping straight into an expensive sandboxed fix
// session. The wait only runs when something was actually re-triggered (non-Actions
// checks have no run id and can't be re-run, so there is nothing to wait for).
func (d *GhDriver) Rerun(failed []Check) error {
	ids := RunIDs(failed)
	for _, id := range ids {
		if _, _, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "gh", "run", "rerun", id, "--failed"); err != nil {
			return fmt.Errorf("gh run rerun %s: %w", id, err)
		}
	}
	if len(ids) > 0 {
		d.sleep(d.pollCfg.interval)
	}
	return nil
}

// Fix fetches the failed-job logs host-side and hands them to the sandbox session.
func (d *GhDriver) Fix(failed []Check) error {
	return d.runFix(d.fetchFailedLogs(RunIDs(failed)))
}

// Push pushes the fix commit (delegated to the injected gitpkg.Push).
func (d *GhDriver) Push() error { return d.push() }

// AwaitHeadRun polls `gh run list --branch <branch> -L 1 --json headSha` until the
// latest run's head commit matches the branch HEAD (resolved with git rev-parse) —
// i.e. CI has actually started a run for the commit the just-pushed fix produced.
// This stops the loop re-acting on the prior run's stale red when a fix lands before
// CI re-evaluates (BEH-493). The branch ref + objects live in the shared .git visible
// from the main checkout, so both reads run against herdPath, never inside a worktree.
func (d *GhDriver) AwaitHeadRun() error {
	head, err := d.branchHead()
	if err != nil {
		return err
	}
	ciHead := func() (string, error) {
		stdout, stderr, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "gh", "run", "list", "--branch", d.branch, "-L", "1", "--json", runHeadJSONField)
		return interpretRunHead(stdout, stderr, err)
	}
	return awaitHeadRun(head, ciHead, d.pollCfg, d.sleep, d.now)
}

// branchHead resolves the PR branch's local HEAD commit — the fix the sandbox just
// committed and Push pushed — via `git rev-parse <branch>` against the main checkout.
func (d *GhDriver) branchHead() (string, error) {
	stdout, stderr, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "git", "rev-parse", d.branch)
	if err != nil {
		if s := strings.TrimSpace(string(stderr)); s != "" {
			return "", fmt.Errorf("git rev-parse %s: %w: %s", d.branch, err, s)
		}
		return "", fmt.Errorf("git rev-parse %s: %w", d.branch, err)
	}
	return strings.TrimSpace(string(stdout)), nil
}

// MergeState polls `gh pr view <branch> --json mergeable,mergeStateStatus` until
// GitHub's async mergeability computation settles, returning the verdict. UNKNOWN
// is retried within the poll budget (not treated as terminal); a persistently
// UNKNOWN window returns MergeUnknown so the watch degrades rather than blocking
// a green PR (BEH-484).
func (d *GhDriver) MergeState() (MergeVerdict, error) {
	fetch := func() (MergeVerdict, error) {
		stdout, stderr, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "gh", "pr", "view", d.branch, "--json", mergeJSONFields)
		return interpretMergeOutput(stdout, stderr, err)
	}
	return pollMergeState(fetch, d.pollCfg, d.sleep, d.now)
}

// fetchFailedLogs concatenates `gh run view <id> --log-failed` for each failing
// run, truncated to the configured tail. gh's own error is folded into the text
// (rather than aborting) so the agent still gets whatever logs were retrievable
// — a partial log beats no log when diagnosing. The scaffolding markers here (the
// run header, the no-runs sentinel, the "could not fully fetch" fold-in) are what
// prompt.logsUnfetchable keys off to detect an empty payload — keep them in sync.
func (d *GhDriver) fetchFailedLogs(runIDs []string) string {
	if len(runIDs) == 0 {
		return NoRunsSentinel
	}
	var b strings.Builder
	for _, id := range runIDs {
		fmt.Fprintf(&b, RunHeaderMarker+"%s (failed steps) =====\n", id)
		stdout, _, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "gh", "run", "view", id, "--log-failed")
		b.Write(stdout)
		if err != nil {
			fmt.Fprintf(&b, "\n"+FetchErrorMarker+"%s: %v)\n", id, err)
		}
		b.WriteString("\n")
	}
	return truncateLogs(b.String(), d.logTailBytes)
}

// interpretChecksOutput turns a `gh pr checks --json` invocation into checks.
// gh exits non-zero when checks fail but still prints the JSON to stdout, so a
// parseable stdout wins regardless of the exit code; gh's error is surfaced only
// when stdout has no usable JSON (a real gh failure — auth, bad branch).
func interpretChecksOutput(stdout, stderr []byte, runErr error) ([]Check, error) {
	if checks, err := ParseChecks(stdout); err == nil {
		return checks, nil
	}
	if runErr != nil {
		s := strings.TrimSpace(string(stderr))
		// gh exits non-zero with this message in the brief window before Actions
		// registers the run — transient, not a real failure.
		if strings.Contains(strings.ToLower(s), "no checks reported") {
			return nil, errNoChecksYet
		}
		// A 403 "Resource not accessible" means the token cannot read check runs
		// at all (fine-grained PAT) — degrade, don't surface as a real failure.
		if isUnobservableErr(s) {
			return nil, errChecksUnobservable
		}
		if s != "" {
			return nil, fmt.Errorf("gh pr checks failed: %w: %s", runErr, s)
		}
		return nil, fmt.Errorf("gh pr checks failed: %w", runErr)
	}
	return nil, fmt.Errorf("gh pr checks returned no parseable JSON: %q", strings.TrimSpace(string(stdout)))
}

// truncateLogs keeps the last `limit` bytes of s (where CI failures surface),
// prefixed with a marker noting the head was dropped. Short logs pass through
// unchanged.
func truncateLogs(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return fmt.Sprintf(TruncationHeadMarker+" %d earlier bytes "+TruncationWord+" …]\n%s", len(s)-limit, s[len(s)-limit:])
}
