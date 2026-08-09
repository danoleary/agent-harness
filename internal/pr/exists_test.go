package pr

import (
	"errors"
	"testing"
)

func fixedState(state string, err error) stateReader {
	return func(_, _ string) (string, error) { return state, err }
}

// A branch whose PR is OPEN or MERGED has escaped the worktree — the PR captures
// the work, so the caller is free to reap. CLOSED counts too: the branch reached a
// PR and a human closed it, which is a decision, not lost work.
func TestExistsCountsEveryTerminalPRState(t *testing.T) {
	for _, state := range []string{"OPEN", "MERGED", "CLOSED"} {
		if !exists("/herd", "feat/beh-783", fixedState(state, nil)) {
			t.Errorf("state %q: Exists = false, want true", state)
		}
	}
}

// The BEH-783 strand: the branch is on the remote but `gh pr create` timed out, so
// gh finds no PR and exits non-zero. The worktree must survive — it is the only
// input the loop's committed-fix recovery has.
func TestExistsTreatsAMissingPRAsNoPR(t *testing.T) {
	if exists("/herd", "feat/beh-783", fixedState("", errors.New("no pull requests found for branch"))) {
		t.Fatal("Exists = true for a branch with no PR, want false")
	}
}

// A gh failure (auth prompt, network, timeout) must not be read as "a PR exists" —
// that would reap the worktree on a transient blip. Err toward keeping it.
func TestExistsTreatsAnUnrecognisedStateAsNoPR(t *testing.T) {
	if exists("/herd", "feat/beh-783", fixedState("", nil)) {
		t.Fatal("Exists = true for empty gh output, want false")
	}
}

func TestOpenExistsOnlyAcceptsOpen(t *testing.T) {
	if !openExists("/herd", "feat/beh-783", fixedState("OPEN", nil)) {
		t.Fatal("OpenExists = false for an OPEN PR, want true")
	}
	for _, state := range []string{"MERGED", "CLOSED", ""} {
		if openExists("/herd", "feat/beh-783", fixedState(state, nil)) {
			t.Errorf("state %q: OpenExists = true, want false", state)
		}
	}
	if openExists("/herd", "feat/beh-783", fixedState("OPEN", errors.New("gh: boom"))) {
		t.Fatal("OpenExists = true despite a gh error, want false")
	}
}
