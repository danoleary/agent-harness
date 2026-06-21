package ci

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MergeStatus is the merge-state half of a PR head, as `gh pr view <branch>
// --json mergeable,mergeStateStatus` reports it. GitHub computes both fields
// asynchronously, so they can read UNKNOWN for a short window after a push.
type MergeStatus struct {
	// Mergeable is GitHub's coarse mergeability: MERGEABLE / CONFLICTING / UNKNOWN.
	Mergeable string `json:"mergeable"`
	// MergeStateStatus is the finer state: CLEAN / DIRTY / BEHIND / BLOCKED /
	// UNSTABLE / HAS_HOOKS / DRAFT / UNKNOWN. DIRTY is the conflict-with-base case.
	MergeStateStatus string `json:"mergeStateStatus"`
}

// ParseMergeStatus decodes the single JSON object `gh pr view --json
// mergeable,mergeStateStatus` writes to stdout.
func ParseMergeStatus(raw []byte) (MergeStatus, error) {
	var s MergeStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return MergeStatus{}, err
	}
	return s, nil
}

// MergeVerdict classifies a PR's mergeability against base.
type MergeVerdict int

const (
	// MergeUnknown — GitHub has not finished computing mergeability yet
	// (mergeable=UNKNOWN); the poller should retry rather than treat it as terminal.
	MergeUnknown MergeVerdict = iota
	// MergeClean — the PR merges cleanly (or is in a non-conflict state like BEHIND
	// / BLOCKED that is not the harness's concern here).
	MergeClean
	// MergeConflicting — the PR conflicts with base (mergeable=CONFLICTING or
	// mergeStateStatus=DIRTY): green checks notwithstanding, it is not shippable.
	MergeConflicting
)

// mergeJSONFields is the `gh pr view --json` field set the merge-state poll reads.
const mergeJSONFields = "mergeable,mergeStateStatus"

// interpretMergeOutput turns a `gh pr view --json mergeable,mergeStateStatus`
// invocation into a verdict. A parseable stdout wins regardless of exit code
// (mirrors interpretChecksOutput); gh's error is surfaced only when stdout has no
// usable JSON (a real gh failure — auth, bad branch), which the caller degrades
// to a green pass rather than blocking on.
func interpretMergeOutput(stdout, stderr []byte, runErr error) (MergeVerdict, error) {
	if s, err := ParseMergeStatus(stdout); err == nil {
		return ClassifyMergeState(s), nil
	}
	if runErr != nil {
		if msg := strings.TrimSpace(string(stderr)); msg != "" {
			return MergeUnknown, fmt.Errorf("gh pr view failed: %w: %s", runErr, msg)
		}
		return MergeUnknown, fmt.Errorf("gh pr view failed: %w", runErr)
	}
	return MergeUnknown, fmt.Errorf("gh pr view returned no parseable JSON: %q", strings.TrimSpace(string(stdout)))
}

// pollMergeState re-runs fetch until the merge verdict settles (Clean or
// Conflicting), sleeping `interval` between UNKNOWN re-checks, bounded by
// `budget`. GitHub computes mergeability asynchronously and returns UNKNOWN
// briefly after a push, so UNKNOWN is "keep waiting", not terminal. If the
// budget is spent while still UNKNOWN it returns MergeUnknown with no error:
// an indeterminate state must not block an otherwise-green PR (the caller
// degrades). A fetch error aborts immediately (a real gh failure). The clock is
// injected so the timing is deterministic in tests (mirrors poll).
func pollMergeState(fetch func() (MergeVerdict, error), cfg pollConfig, sleep func(time.Duration), now func() time.Time) (MergeVerdict, error) {
	deadline := now().Add(cfg.budget)
	for {
		v, err := fetch()
		if err != nil {
			return MergeUnknown, err
		}
		if v != MergeUnknown {
			return v, nil
		}
		// Still computing — stop if the next interval would carry us past budget.
		if !now().Add(cfg.interval).Before(deadline) {
			return MergeUnknown, nil
		}
		sleep(cfg.interval)
	}
}

// ClassifyMergeState reduces a MergeStatus to a verdict. A definite conflict
// (CONFLICTING / DIRTY) wins; otherwise an unresolved UNKNOWN means "retry";
// anything else is treated as clean (not a conflict, so not our concern).
func ClassifyMergeState(s MergeStatus) MergeVerdict {
	if s.Mergeable == "CONFLICTING" || s.MergeStateStatus == "DIRTY" {
		return MergeConflicting
	}
	if s.Mergeable == "UNKNOWN" {
		return MergeUnknown
	}
	return MergeClean
}
