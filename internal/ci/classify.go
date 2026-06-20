package ci

// gh's check buckets — the coarse status `gh pr checks --json bucket` reports.
// These are the literal strings gh emits; the harness keys its verdict off them.
const (
	BucketPass     = "pass"
	BucketFail     = "fail"
	BucketPending  = "pending"
	BucketSkipping = "skipping"
	BucketCancel   = "cancel"
)

// Verdict is the terminal-state classification of a PR's whole set of checks.
type Verdict int

const (
	// Pending — at least one check has not reached a terminal state yet; the
	// poller must keep waiting (this outranks an already-red check, since a run
	// isn't terminal until everything settles).
	Pending Verdict = iota
	// Passed — every check reached a terminal state and none failed (pass or
	// skipping only). An empty check set is Passed: nothing red to wait on.
	Passed
	// Failed — every check is terminal and at least one failed or was cancelled.
	Failed
)

func (v Verdict) String() string {
	switch v {
	case Pending:
		return "pending"
	case Passed:
		return "passed"
	case Failed:
		return "failed"
	default:
		return "unknown"
	}
}

// Classify reduces a set of checks to a single terminal verdict. Pending wins
// over Failed (the run isn't terminal until nothing is in-flight); among terminal
// sets, any fail/cancel makes the whole run Failed, else it Passed.
func Classify(checks []Check) Verdict {
	failed := false
	for _, c := range checks {
		switch c.Bucket {
		case BucketPending:
			return Pending
		case BucketFail, BucketCancel:
			failed = true
		}
	}
	if failed {
		return Failed
	}
	return Passed
}
