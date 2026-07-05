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

// errChecksUnauthenticated marks the case where `gh pr checks` is rejected with an
// HTTP 401 / "Bad credentials" — the poll path's token is missing, stale, or wrong
// even though `git push` + `gh pr create` succeeded moments earlier with the harness
// auth (BEH-627, the BEH-625 false-negative). Like errChecksUnobservable this is an
// environment/credentials problem, NOT a red build: the diff is pushed and gate-green,
// the harness simply can't authenticate to *read* CI status. The watch must fail soft
// (leave the open PR for a human) rather than report "CI did not go green".
var errChecksUnauthenticated = errors.New("CI checks not readable: gh not authenticated (HTTP 401 / bad credentials) — PR was pushed, check CI manually")

// isUnobservableErr reports whether gh's stderr carries GitHub's permission-denied
// signature for the Checks API. GitHub returns "Resource not accessible by …" for
// a token lacking the permission — distinct from "authentication required" / "Bad
// credentials" (a missing/invalid token) or "no checks reported" (transient).
func isUnobservableErr(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "resource not accessible")
}

// isUnauthenticatedErr reports whether gh's stderr carries GitHub's bad-credentials
// signature: an HTTP 401, the literal "Bad credentials", or gh's "gh auth login"
// re-auth hint. Distinct from isUnobservableErr (a token that authenticates but lacks
// the Checks permission — a 403 "resource not accessible") and from a transient "no
// checks reported": this is a token that GitHub rejected outright (BEH-627).
func isUnauthenticatedErr(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "bad credentials") ||
		strings.Contains(s, "http 401") ||
		strings.Contains(s, "gh auth login")
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
// Actions runs at all, or a fetch error folded into the text). fetchFailedLogs now
// reports unfetchability directly via its second return, so the prompt no longer
// re-parses these — but they remain the single source of truth for the payload
// format the driver emits and its tests assert against, exported so a marker rename
// here moves the producer and those fixtures together (BEH-560/BEH-563).
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
	// + fix + commit, given the failing CI logs and whether those logs were actually
	// fetchable. An unfetchable log is the strongest flake signal (BEH-558), so the
	// prompt steers differently when logAvailable is false. Returns non-nil on
	// session failure.
	runFix func(ciLogs string, logAvailable bool) error
	// push pushes the new fix commit to the PR branch (gitpkg.Push).
	push func() error
	// rebase rebases the branch onto the latest origin/main and, if clean, re-pushes
	// (force-with-lease) — the host-side git effect wired by the stages layer. It is a
	// callback for the same reason push/runFix are: so this package needn't import
	// internal/git (BEH-570).
	rebase func() (RebaseVerdict, error)
	// diffEmpty reports whether the pushed branch makes zero net change against
	// origin/main (gitpkg.BranchDiffEmpty over the worktree). Injected like the
	// others so this package needn't import internal/git; powers the zero-net-diff
	// watch short-circuit (BEH-602).
	diffEmpty func() bool
	// docsOnly reports whether the pushed branch's net diff touches ONLY docs/prose
	// paths no gate or CI job reads (gitpkg.BranchDocsOnly over the worktree). Injected
	// like diffEmpty; powers the docs-only watch short-circuit (BEH-687).
	docsOnly func() bool

	// fetchRunLog fetches one failing run's log (`gh run view <id> --log-failed`).
	// Injected so the log-availability logic is unit-testable without shelling out.
	fetchRunLog func(id string) ([]byte, error)

	sleep func(time.Duration)
	now   func() time.Time
}

// NewGhDriver builds the production Driver. ghTimeout bounds each individual gh
// call; cfg supplies the poll cadence/budget; runFix and push are the sandbox +
// remote effects the cmd provides.
func NewGhDriver(herdPath, branch string, cfg Config, ghTimeout time.Duration, runFix func(ciLogs string, logAvailable bool) error, push func() error, rebase func() (RebaseVerdict, error), diffEmpty func() bool, docsOnly func() bool) *GhDriver {
	d := &GhDriver{
		herdPath:     herdPath,
		branch:       branch,
		pollCfg:      pollConfig{interval: cfg.PollInterval, budget: cfg.PollBudget, maxBudget: cfg.PollMaxBudget, stall: cfg.PollStall},
		ghTimeout:    ghTimeout,
		logTailBytes: defaultLogTailBytes,
		runFix:       runFix,
		push:         push,
		rebase:       rebase,
		diffEmpty:    diffEmpty,
		docsOnly:     docsOnly,
		sleep:        time.Sleep,
		now:          time.Now,
	}
	d.fetchRunLog = func(id string) ([]byte, error) {
		stdout, _, err := proc.OutputInDir(d.ghTimeout, d.herdPath, "gh", "run", "view", id, "--log-failed")
		return stdout, err
	}
	return d
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

// Fix fetches the failed-job logs host-side and hands them — plus whether they
// were actually fetchable — to the sandbox session.
func (d *GhDriver) Fix(failed []Check) error {
	logs, available := d.fetchFailedLogs(RunIDs(failed))
	return d.runFix(logs, available)
}

// Push pushes the fix commit (delegated to the injected gitpkg.Push).
func (d *GhDriver) Push() error { return d.push() }

// RebaseOntoBase rebases the branch onto the latest origin/main and re-pushes a
// clean replay (delegated to the injected host-side git effect, BEH-570).
func (d *GhDriver) RebaseOntoBase() (RebaseVerdict, error) { return d.rebase() }

// DiffEmpty reports whether the pushed branch makes zero net change against
// origin/main (delegated to the injected gitpkg.BranchDiffEmpty, BEH-602).
func (d *GhDriver) DiffEmpty() bool { return d.diffEmpty() }

// DocsOnly reports whether the pushed branch's net diff touches only docs/prose
// paths no gate or CI job reads (delegated to the injected gitpkg.BranchDocsOnly,
// BEH-687).
func (d *GhDriver) DocsOnly() bool { return d.docsOnly() }

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
// — a partial log beats no log when diagnosing. The second return reports whether
// any usable log was actually fetched: false when there is no backing Actions run,
// or when every fetch errored (the log expired / the step was an infra-level kill).
// That flag is the strongest flake signal the fix prompt has (BEH-558). The payload
// is built from the exported marker constants (run header, no-runs sentinel,
// folded-in fetch error) so a rename moves the producer and its test fixtures
// together (BEH-563).
func (d *GhDriver) fetchFailedLogs(runIDs []string) (string, bool) {
	if len(runIDs) == 0 {
		return NoRunsSentinel, false
	}
	var b strings.Builder
	available := false
	for _, id := range runIDs {
		fmt.Fprintf(&b, RunHeaderMarker+"%s (failed steps) =====\n", id)
		stdout, err := d.fetchRunLog(id)
		b.Write(stdout)
		if err != nil {
			fmt.Fprintf(&b, "\n"+FetchErrorMarker+"%s: %v)\n", id, err)
		} else if len(strings.TrimSpace(string(stdout))) > 0 {
			available = true
		}
		b.WriteString("\n")
	}
	return truncateLogs(b.String(), d.logTailBytes), available
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
		// An HTTP 401 / "Bad credentials" means the poll path's token was rejected
		// outright (missing/stale/wrong) even though push + PR create just succeeded —
		// a credentials/environment problem, not a red build (BEH-627). Degrade rather
		// than report the pushed, gate-green PR as a CI failure.
		if isUnauthenticatedErr(s) {
			return nil, errChecksUnauthenticated
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
