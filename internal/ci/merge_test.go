package ci

import (
	"errors"
	"strings"
	"testing"
)

func TestInterpretMergeOutputClassifiesConflict(t *testing.T) {
	// gh pr view writes the JSON object on stdout; a CONFLICTING PR classifies
	// straight to MergeConflicting regardless of exit code.
	v, err := interpretMergeOutput([]byte(`{"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}`), nil, nil)
	if err != nil {
		t.Fatalf("interpretMergeOutput: %v", err)
	}
	if v != MergeConflicting {
		t.Fatalf("verdict = %v, want MergeConflicting", v)
	}
}

func TestInterpretMergeOutputSurfacesRealGhFailure(t *testing.T) {
	// No parseable JSON + a gh error → surface it (so the loop aborts rather than
	// spinning). greenOutcome degrades such an error to a green pass.
	_, err := interpretMergeOutput([]byte(""), []byte("gh: authentication required"), errors.New("exit status 4"))
	if err == nil {
		t.Fatal("expected an error when stdout has no JSON and gh failed")
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("error should surface gh's stderr, got %q", err.Error())
	}
}

func TestPollMergeStateToleratesPersistentUnknown(t *testing.T) {
	// If GitHub never finishes computing within budget, the loop must NOT block —
	// it returns MergeUnknown (no error) so the caller can ship a green PR rather
	// than fail it on an indeterminate merge state.
	clock := newFakeClock()
	fetch := func() (MergeVerdict, error) { return MergeUnknown, nil }
	v, err := pollMergeState(fetch, testPollCfg(), clock.sleep, clock.now)
	if err != nil {
		t.Fatalf("pollMergeState: %v", err)
	}
	if v != MergeUnknown {
		t.Fatalf("verdict = %v, want MergeUnknown on budget exhaustion", v)
	}
}

func TestPollMergeStateWaitsThroughUnknownThenSettles(t *testing.T) {
	// GitHub returns UNKNOWN while it computes mergeability; the loop must retry
	// rather than treat that as terminal, then report the settled conflict.
	clock := newFakeClock()
	calls := 0
	fetch := func() (MergeVerdict, error) {
		calls++
		if calls <= 2 {
			return MergeUnknown, nil
		}
		return MergeConflicting, nil
	}
	v, err := pollMergeState(fetch, testPollCfg(), clock.sleep, clock.now)
	if err != nil {
		t.Fatalf("pollMergeState: %v", err)
	}
	if v != MergeConflicting {
		t.Fatalf("verdict = %v, want MergeConflicting once it settled", v)
	}
	if calls != 3 {
		t.Fatalf("fetched %d times, want 3 (waited through 2 UNKNOWN)", calls)
	}
}

func TestParseMergeStatusDecodesGhObject(t *testing.T) {
	// `gh pr view --json mergeable,mergeStateStatus` writes a single JSON object.
	s, err := ParseMergeStatus([]byte(`{"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY"}`))
	if err != nil {
		t.Fatalf("ParseMergeStatus: %v", err)
	}
	if s.Mergeable != "CONFLICTING" || s.MergeStateStatus != "DIRTY" {
		t.Fatalf("parsed %+v, want CONFLICTING/DIRTY", s)
	}
}

func TestClassifyMergeStateFlagsConflict(t *testing.T) {
	// GitHub reports a PR that conflicts with base as mergeable=CONFLICTING /
	// mergeStateStatus=DIRTY. That is the state the harness must catch.
	got := ClassifyMergeState(MergeStatus{Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"})
	if got != MergeConflicting {
		t.Fatalf("ClassifyMergeState(CONFLICTING/DIRTY) = %v, want MergeConflicting", got)
	}
}

func TestClassifyMergeStateCleanAndUnknown(t *testing.T) {
	// A mergeable PR (MERGEABLE/CLEAN) is clean; non-conflict states like BEHIND
	// are not the harness's concern, so they classify clean too. UNKNOWN means
	// GitHub is still computing — the poller must keep waiting, not ship/block.
	cases := []struct {
		name string
		in   MergeStatus
		want MergeVerdict
	}{
		{"mergeable", MergeStatus{Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}, MergeClean},
		{"behind is not a conflict", MergeStatus{Mergeable: "MERGEABLE", MergeStateStatus: "BEHIND"}, MergeClean},
		{"unknown retries", MergeStatus{Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN"}, MergeUnknown},
	}
	for _, c := range cases {
		if got := ClassifyMergeState(c.in); got != c.want {
			t.Fatalf("%s: ClassifyMergeState(%+v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

func TestInterpretMergeOutputClassifiesClean(t *testing.T) {
	// A clean, mergeable PR parses straight to MergeClean with no error, so
	// greenOutcome declares the pass.
	v, err := interpretMergeOutput([]byte(`{"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN"}`), nil, nil)
	if err != nil {
		t.Fatalf("interpretMergeOutput: %v", err)
	}
	if v != MergeClean {
		t.Fatalf("verdict = %v, want MergeClean", v)
	}
}
