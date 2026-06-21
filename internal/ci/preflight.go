package ci

// ProbeRunner runs a command and returns its combined output (mirrors
// sandbox.ProbeRunner). cmd/review injects one bound to the herd checkout dir so
// gh's {owner}/{repo} placeholders resolve against the right repository.
type ProbeRunner func(name string, args ...string) ([]byte, error)

// checkRunsProbeArgs probes the Checks API on main's HEAD. {owner}/{repo} are gh
// placeholders resolved from the checkout the runner runs in. The Checks API
// (commits/<ref>/check-runs) is the *exact* resource a fine-grained PAT is blind
// to — probing the Actions or statuses endpoints instead would pass a blind
// token, since those use different permissions (BEH-476).
var checkRunsProbeArgs = []string{"api", "repos/{owner}/{repo}/commits/main/check-runs"}

// PreflightMessage is the actionable warning surfaced when GH_TOKEN cannot read
// check runs. The PR will still ship (the watch degrades gracefully) — this just
// tells the operator early, before the expensive run, why CI won't be watched.
const PreflightMessage = "GH_TOKEN cannot read check runs — the post-PR CI-watch needs a classic `repo`-scoped PAT (SSO-authorized for the org); fine-grained PATs lack the Checks permission. The PR will still ship, but CI won't be watched or auto-fixed (BEH-476)."

// ChecksReadable probes whether GH_TOKEN can read the Checks API before a run
// commits to the CI-watch phase. It returns (false, PreflightMessage) only when
// the probe shows GitHub's permission ceiling ("Resource not accessible"); a
// clean probe or any unrelated/transient failure (network, rate limit) returns
// readable=true, since blocking the run on a flaky probe would be worse than the
// watch's own degrade path. The runner is injected so this stays unit-testable.
func ChecksReadable(run ProbeRunner) (readable bool, detail string) {
	out, err := run("gh", checkRunsProbeArgs...)
	if err == nil {
		return true, ""
	}
	if isUnobservableErr(string(out)) {
		return false, PreflightMessage
	}
	return true, ""
}
