package prompt

import (
	"strings"
	"testing"
)

// #86: the harness never updates local `main`, so once a sibling ticket merges
// mid-run `main...HEAD` drags that sibling's commits into the session's scope. The
// review and retrospective envelopes name the exact commit the branch was cut from
// (or last rebased onto) and scope the diff to it.
func TestReviewAndRetrospectiveScopeTheDiffToTheBranchBase(t *testing.T) {
	const base = "0123456789abcdef0123456789abcdef01234567"
	for name, p := range map[string]string{
		"review":        For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, Body: exampleBody(t, "review"), Base: base}),
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, Body: exampleBody(t, "retro"), Base: base}),
	} {
		for _, want := range []string{"git diff " + base + "...HEAD", "git log " + base + "..HEAD", "stale"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt missing %q", name, want)
			}
		}
	}
}

// A base the host could not resolve still steers off the stale local `main`: the
// session falls back to origin/main, whose three-dot diff finds the same fork point.
func TestBranchBaseFallsBackToOriginMainWhenUnresolved(t *testing.T) {
	for name, p := range map[string]string{
		"review":        For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
	} {
		if !strings.Contains(p, "git diff origin/main...HEAD") {
			t.Errorf("%s prompt does not fall back to origin/main", name)
		}
	}
}

// No harness-owned envelope text points a session at local `main`.
func TestNoEnvelopeDiffsAgainstLocalMain(t *testing.T) {
	for name, p := range map[string]string{
		"review":        For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"resumed":       For(Implement, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, Resume: ResumedBranch}),
	} {
		for _, stale := range []string{"`git log main..HEAD`", "`git diff main...HEAD`"} {
			if strings.Contains(p, stale) {
				t.Errorf("%s prompt still names %s", name, stale)
			}
		}
	}
}
