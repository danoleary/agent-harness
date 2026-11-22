package ci

import (
	"fmt"
	"strings"
)

// FailedChecks returns just the checks that failed or were cancelled — the set
// the harness fetches logs for, re-runs, and reports. Skipped/passed checks are
// dropped (gh marks them pass/skipping, never fail).
func FailedChecks(checks []Check) []Check {
	var failed []Check
	for _, c := range checks {
		if c.Bucket == BucketFail || c.Bucket == BucketCancel {
			failed = append(failed, c)
		}
	}
	return failed
}

// Summarize renders the failing checks for the operator report the tool prints
// on budget/attempt exhaustion — name, bucket, and the run link so a human can
// jump straight to the logs (the "print the failing checks + logs pointer"
// acceptance criterion; surface diagnostics, not bare exit codes — cf. BEH-404).
func Summarize(failed []Check) string {
	var b strings.Builder
	for _, c := range failed {
		fmt.Fprintf(&b, "  ✗ %s (%s) — %s\n", c.Name, c.Bucket, c.Link)
	}
	return b.String()
}

// flakeNames joins the checks' names for a one-line run-log reason, in poll
// order, each followed by its failing step where steps has one, e.g.
// "lint / Run golangci-lint, e2e" (#371).
func flakeNames(checks []Check, steps map[string]string) string {
	names := make([]string, len(checks))
	for i, c := range checks {
		names[i] = c.Name
		if step := steps[c.Name]; step != "" {
			names[i] += " / " + step
		}
	}
	return strings.Join(names, ", ")
}
