package ci

import "regexp"

// runIDRE pulls the run id out of a GitHub Actions check link, e.g.
// https://github.com/owner/repo/actions/runs/100/job/2  → 100
// https://github.com/owner/repo/actions/runs/55          → 55
// Non-Actions links (external status checks, empty links) don't match and are
// skipped — the harness can only `gh run` against Actions runs.
var runIDRE = regexp.MustCompile(`github\.com/[^/]+/[^/]+/actions/runs/(\d+)`)

// RunIDs returns the unique GitHub Actions run ids backing the given checks, in
// first-seen order. Several failing jobs often share one run (one workflow, many
// jobs), so this dedupes — the harness fetches `--log-failed` and re-runs once
// per run, not once per job.
func RunIDs(checks []Check) []string {
	var ids []string
	seen := make(map[string]bool)
	for _, c := range checks {
		m := runIDRE.FindStringSubmatch(c.Link)
		if m == nil {
			continue
		}
		if id := m[1]; !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}
