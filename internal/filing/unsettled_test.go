package filing

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/tracker"
)

// staleBaseRed is the open base-gates-red issue of #311: filed while main was
// red, and still open on a run that saw the base gates pass.
func staleBaseRed() *fakeSearcher {
	return &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "#280", Title: `main fails gate "check" before any change, so every ticket on it is held`, Key: "base-gates-red"},
		{Identifier: "#281", Title: "Build OOMs", Key: "sandbox-build-oom"},
	}}
}

const baseRedDropbox = `[{"title":"main fails gate \"check\" before any change, so every ticket on it is held","body":"red again","key":"base-gates-red","audience":"project"}]`

// #311: on a run that saw the base gates pass, the open base-gates-red issue is
// not a settled class, so the retrospective is not told to reuse it.
func TestAlreadyFiledLeavesOutAnUnsettledKey(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, baseRedDropbox)

	got := Router{Project: sink(nil, staleBaseRed(), nil), Unsettled: []string{"base-gates-red"}, Log: &recorder{}}.AlreadyFiled(dir, "team-uuid")

	if len(got) != 1 || got[0].Key != "sandbox-build-oom" {
		t.Errorf("settled = %+v, want only sandbox-build-oom: base-gates-red is unsettled on a green base", got)
	}
}

// #311: a retrospective on a green-base run that reports a base-gates-red
// finding files a new issue rather than recording a recurrence on the stale one.
func TestRouteFilesAnUnsettledKeyAfreshInsteadOfDeduping(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, baseRedDropbox)
	filer := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "#400"}}}}
	bumper := &fakeRecorder{}
	matcher := &fakeMatcher{matchTo: "#280"}

	Router{Project: sink(filer, staleBaseRed(), bumper), Matcher: matcher, Unsettled: []string{"base-gates-red"}, Log: &recorder{}}.Route(dir, "team-uuid", "#311")

	if len(filer.calls) != 1 {
		t.Fatalf("filed %d, want the base-gates-red finding filed as a new issue", len(filer.calls))
	}
	if len(bumper.bumped) != 0 {
		t.Errorf("recurrences recorded on %v, want none: the open issue is stale", bumper.bumped)
	}
	for _, e := range matcher.lastOpen {
		if e.Identifier == "#280" {
			t.Errorf("the semantic pass was offered the stale issue %s", e.Identifier)
		}
	}
}

// With no unsettled key — a red base, or no verdict — the open issue still
// dedups as it always has.
func TestRouteStillDedupsTheOpenBaseRedIssueWhenSettled(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, baseRedDropbox)
	filer := &fakeFiler{}
	bumper := &fakeRecorder{count: 2}
	r := Router{Project: sink(filer, staleBaseRed(), bumper), Log: &recorder{}}

	settled := r.AlreadyFiled(dir, "team-uuid")
	r.Route(dir, "team-uuid", "#311")

	if len(settled) != 2 {
		t.Errorf("settled = %+v, want both open classes", settled)
	}
	if len(filer.calls) != 0 || len(bumper.bumped) != 1 || bumper.bumped[0] != "#280" {
		t.Errorf("filed %d, bumped %v; want a recurrence on #280 and nothing filed", len(filer.calls), bumper.bumped)
	}
}
