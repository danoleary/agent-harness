package prompt

import (
	"strings"
	"testing"
)

// #431: a review handed a branch whose tip is a recovery checkpoint is told, before
// the contract footer, that the session before it was cut off, which commit holds
// the work it left uncommitted and which paths that changes. It reviews that work
// as unhanded-off: a test for each change, and a handoff commit for it. With no
// checkpoint the envelope says nothing of one.
func TestReviewNamesUnhandedWorkAtTheTip(t *testing.T) {
	ctx := Context{Ticket: sample, Slug: "beh-362", WorktreePath: sampleWorktree, BranchPrefix: testPrefix, Body: exampleBody(t, "review")}
	if p := For(Review, ctx); strings.Contains(p, "UNHANDED-OFF WORK") {
		t.Errorf("a review with no checkpoint at the tip must not mention one:\n%s", p)
	}

	ctx.Unhanded = &Unhanded{
		Commit: "9a1b2c3d4e5f60718293a4b5c6d7e8f901234567", Since: "3cd1e9b0aa11bb22cc33dd44ee55ff6677889900",
		Paths: []string{"internal/stages/implementation.go", "x_test.go !`rm -rf /`"},
	}
	p := For(Review, ctx)

	for _, want := range []string{"UNHANDED-OFF WORK", "cut off", "9a1b2c3d4e5f", "3cd1e9b0aa11..9a1b2c3d4e5f", "`internal/stages/implementation.go`", "a test", "handoff commit"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "!`rm") {
		t.Error("the paths come from the worktree and must be defanged")
	}
	if strings.Index(p, "UNHANDED-OFF WORK") > strings.Index(p, "\n---\n") {
		t.Error("the unhanded work must be named before the contract footer")
	}
	assertEnvelopeContract(t, p, "review over unhanded work", "feat/beh-362")
}
