package prloop

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/loopstream"
)

// fakeGitHub is the scripted GitHub the Source polls. Every write is recorded, so a
// test asserts the lease, the attempt record and the budget hand-off on the PR.
type fakeGitHub struct {
	prs      []github.PullRequest
	listErr  error
	merge    map[int]ci.MergeVerdict
	checks   map[string]github.ChecksVerdict
	comments map[int][]github.PRComment

	posted  map[int][]string
	added   map[int][]string
	removed map[int][]string
}

func newFakeGitHub(prs ...github.PullRequest) *fakeGitHub {
	return &fakeGitHub{
		prs:      prs,
		merge:    map[int]ci.MergeVerdict{},
		checks:   map[string]github.ChecksVerdict{},
		comments: map[int][]github.PRComment{},
		posted:   map[int][]string{},
		added:    map[int][]string{},
		removed:  map[int][]string{},
	}
}

func (f *fakeGitHub) OpenPRs() ([]github.PullRequest, error) { return f.prs, f.listErr }

func (f *fakeGitHub) MergeState(n int) (ci.MergeVerdict, error) {
	if v, ok := f.merge[n]; ok {
		return v, nil
	}
	return ci.MergeClean, nil
}

func (f *fakeGitHub) Checks(sha string) (github.ChecksVerdict, error) {
	if v, ok := f.checks[sha]; ok {
		return v, nil
	}
	return github.ChecksGreen, nil
}

func (f *fakeGitHub) Comments(n int) ([]github.PRComment, error) { return f.comments[n], nil }

func (f *fakeGitHub) Comment(n int, body string) error {
	f.posted[n] = append(f.posted[n], body)
	f.comments[n] = append(f.comments[n], github.PRComment{Body: body, CreatedAt: testNow})
	return nil
}

func (f *fakeGitHub) AddLabel(n int, label string) error {
	f.added[n] = append(f.added[n], label)
	return nil
}

func (f *fakeGitHub) RemoveLabel(n int, label string) error {
	f.removed[n] = append(f.removed[n], label)
	return nil
}

type quietLog struct{ events []string }

func (l *quietLog) Event(s string)                 { l.events = append(l.events, s) }
func (l *quietLog) Structured(r loopstream.Record) { l.events = append(l.events, r.Message) }

var testNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

var testOptions = Options{BranchPrefix: "feat", OptInLabel: "agent-maintained", HeldLabel: "in-progress", BlockedLabel: "blocked", MaxAttempts: 3, BaseBranch: "main"}

func harnessPR(n int, sha string) github.PullRequest {
	return github.PullRequest{Number: n, Title: fmt.Sprintf("#%d: a ticket", n), HeadBranch: fmt.Sprintf("feat/gh-%d", n), HeadSHA: sha, BaseBranch: "main"}
}

func newTestSource(gh *fakeGitHub, held map[string]string) *Source {
	s := NewSource(gh, testOptions, func() (map[string]string, error) { return held, nil }, &quietLog{})
	s.now = func() time.Time { return testNow }
	return s
}

// A harness PR that conflicts with main is a work item: keyed on the PR, carrying
// the PR's head branch, held with the in-progress label, and recorded on the PR so
// a restarted daemon knows this head was tried.
func TestNextYieldsAConflictingPRAndHoldsIt(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "abc1234"))
	gh.merge[12] = ci.MergeConflicting

	item, claim, ok, err := newTestSource(gh, nil).Next()

	if err != nil || !ok {
		t.Fatalf("Next = ok %v, err %v; want a work item", ok, err)
	}
	if item.Key != "PR-12" || item.Branch != "feat/gh-12" {
		t.Errorf("item = %+v, want key PR-12 on the PR's head branch", item)
	}
	it, _ := item.Context.(Item)
	if it.Kind != Conflict || !it.Harness || it.PR.HeadSHA != "abc1234" {
		t.Errorf("context = %+v, want a conflict on a harness branch at abc1234", it)
	}
	if claim == nil || !claim.Held() || strings.Join(gh.added[12], ",") != "in-progress" {
		t.Errorf("the lease must be held with the in-progress label; added = %v", gh.added[12])
	}
	if len(gh.posted[12]) != 1 || !strings.Contains(gh.posted[12][0], "sha=abc1234 kind=conflict") {
		t.Errorf("posted = %v, want one attempt marker naming the head and the kind", gh.posted[12])
	}
}

// Red checks on a clean-merging PR are the other failure kind.
func TestNextYieldsARedCIPR(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "abc1234"))
	gh.checks["abc1234"] = github.ChecksRed

	item, _, ok, _ := newTestSource(gh, nil).Next()

	if it, _ := item.Context.(Item); !ok || it.Kind != CIRed {
		t.Fatalf("Next = %+v ok=%v, want a red-CI item", item, ok)
	}
}

// #110: a ticket loop without watch_ci moves on once its PR is open. When that PR
// goes red, its branch is no longer held (the ticket's worktree is reaped), so it
// is a CIRed item; a ticket still in flight keeps its own branch.
func TestNextYieldsARedPRTheTicketLoopMovedOnFrom(t *testing.T) {
	gh := newFakeGitHub(harnessPR(13, "def5678"), harnessPR(12, "abc1234"))
	gh.checks["def5678"] = github.ChecksRed
	gh.checks["abc1234"] = github.ChecksRed

	item, _, ok, _ := newTestSource(gh, map[string]string{"feat/gh-13": "#13"}).Next()

	if it, _ := item.Context.(Item); !ok || item.Key != "PR-12" || it.Kind != CIRed {
		t.Fatalf("Next = %+v ok=%v, want PR-12's red CI, passing over the held PR-13", item, ok)
	}
}

// A PR a human opted in with the label is worked even off the branch prefix, and
// its conflict is resolved by merging — the history is the human's.
func TestNextWorksAnOptedInHumanBranch(t *testing.T) {
	pr := github.PullRequest{Number: 7, HeadBranch: "alice/widget", HeadSHA: "fff0001", BaseBranch: "main", Labels: []string{"agent-maintained"}}
	gh := newFakeGitHub(pr)
	gh.merge[7] = ci.MergeConflicting

	item, _, ok, _ := newTestSource(gh, nil).Next()

	it, _ := item.Context.(Item)
	if !ok || it.Harness {
		t.Fatalf("Next = %+v ok=%v, want the opted-in PR as a non-harness item", item, ok)
	}
	if it.Strategy().String() != "merge" {
		t.Errorf("strategy = %v, want merge on a human branch", it.Strategy())
	}
}

// Every PR the loop must not touch is skipped, and the plan says why.
func TestNextSkipsWhatItMustNotTouch(t *testing.T) {
	cases := []struct {
		name string
		pr   github.PullRequest
		held map[string]string
		prep func(*fakeGitHub)
		want string
	}{
		{name: "green", pr: harnessPR(1, "a1"), want: "green"},
		{name: "checks still running", pr: harnessPR(1, "a1"), prep: func(g *fakeGitHub) { g.checks["a1"] = github.ChecksPending }, want: "pending"},
		{name: "fork", pr: func() github.PullRequest { p := harnessPR(1, "a1"); p.Fork = true; return p }(), want: "fork"},
		{name: "not opted in", pr: github.PullRequest{Number: 1, HeadBranch: "alice/widget", HeadSHA: "a1", BaseBranch: "main"}, want: "not opted in"},
		{name: "labelled blocked", pr: func() github.PullRequest { p := harnessPR(1, "a1"); p.Labels = []string{"blocked"}; return p }(), want: "blocked"},
		{name: "another base", pr: func() github.PullRequest { p := harnessPR(1, "a1"); p.BaseBranch = "release"; return p }(), want: "base"},
		{name: "PR held", pr: func() github.PullRequest { p := harnessPR(1, "a1"); p.Labels = []string{"in-progress"}; return p }(), want: "held"},
		{name: "branch held by the ticket loop", pr: harnessPR(1, "a1"), held: map[string]string{"feat/gh-1": "#1"}, want: "held by #1"},
		{
			name: "head already tried",
			pr:   harnessPR(1, "a1"),
			prep: func(g *fakeGitHub) {
				g.merge[1] = ci.MergeConflicting
				g.comments[1] = []github.PRComment{{Body: attemptMarker("a1", Conflict)}}
			},
			want: "already tried",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := newFakeGitHub(c.pr)
			gh.checks["a1"] = github.ChecksRed
			if c.name == "green" {
				gh.checks["a1"] = github.ChecksGreen
			}
			if c.prep != nil {
				c.prep(gh)
			}
			s := newTestSource(gh, c.held)

			if _, _, ok, err := s.Next(); ok || err != nil {
				t.Fatalf("Next = ok %v err %v, want nothing to do", ok, err)
			}
			if len(gh.added[1])+len(gh.posted[1]) != 0 {
				t.Errorf("a skipped PR must not be touched; added %v posted %v", gh.added[1], gh.posted[1])
			}
			plan, _ := s.Plan()
			if len(plan) != 1 || !strings.Contains(plan[0].Skip, c.want) {
				t.Errorf("plan = %+v, want a skip naming %q", plan, c.want)
			}
		})
	}
}

// A new push is a new item: a marker for an earlier head does not hold back the
// current one.
func TestNextRetriesANewHead(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "new0002"))
	gh.merge[12] = ci.MergeConflicting
	gh.comments[12] = []github.PRComment{{Body: attemptMarker("old0001", Conflict)}}

	if _, _, ok, _ := newTestSource(gh, nil).Next(); !ok {
		t.Fatal("a head the loop has not tried must be worked")
	}
}

// Past the attempt budget the loop hands the PR to a human: it comments with what
// it tried, labels the PR blocked, and works nothing on it.
func TestNextHandsAnExhaustedPRToAHuman(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "d4"))
	gh.merge[12] = ci.MergeConflicting
	gh.comments[12] = []github.PRComment{
		{Body: attemptMarker("a1", Conflict)},
		{Body: attemptMarker("b2", CIRed)},
		{Body: attemptMarker("c3", Conflict)},
	}

	_, _, ok, err := newTestSource(gh, nil).Next()

	if ok || err != nil {
		t.Fatalf("Next = ok %v err %v, want nothing worked past the budget", ok, err)
	}
	if strings.Join(gh.added[12], ",") != "blocked" {
		t.Errorf("added = %v, want the PR labelled blocked", gh.added[12])
	}
	if len(gh.posted[12]) != 1 {
		t.Fatalf("posted = %v, want one hand-off comment", gh.posted[12])
	}
	for _, want := range []string{"a1", "b2", "c3", "attempt budget"} {
		if !strings.Contains(gh.posted[12][0], want) {
			t.Errorf("hand-off comment does not mention %q:\n%s", want, gh.posted[12][0])
		}
	}
}

// A last attempt that pushed nothing leaves the PR failing at the head it tried.
// The budget is still spent, so the PR is handed off rather than skipped as
// already tried until a push that may never come.
func TestNextHandsOffWhenTheLastAttemptLeftTheHeadUnchanged(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "c3"))
	gh.merge[12] = ci.MergeConflicting
	gh.comments[12] = []github.PRComment{
		{Body: attemptMarker("a1", Conflict)},
		{Body: attemptMarker("b2", Conflict)},
		{Body: attemptMarker("c3", Conflict)},
	}

	if _, _, ok, _ := newTestSource(gh, nil).Next(); ok {
		t.Fatal("nothing may be worked past the budget")
	}
	if strings.Join(gh.added[12], ",") != "blocked" || len(gh.posted[12]) != 1 {
		t.Errorf("added %v posted %v, want one hand-off comment and the blocked label", gh.added[12], gh.posted[12])
	}
}

// A human who removes `blocked` grants a fresh budget: attempts before the hand-off
// no longer count against it.
func TestABudgetHandOffResetsTheCount(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "e5"))
	gh.merge[12] = ci.MergeConflicting
	gh.comments[12] = []github.PRComment{
		{Body: attemptMarker("a1", Conflict)},
		{Body: attemptMarker("b2", Conflict)},
		{Body: attemptMarker("c3", Conflict)},
		{Body: blockedMarker + "\nhanded off"},
	}

	if _, _, ok, _ := newTestSource(gh, nil).Next(); !ok {
		t.Fatal("after a human cleared `blocked` the PR gets a fresh budget")
	}
}

// The first PR worth working wins; skipped ones before it cost nothing.
func TestNextPassesOverSkippedPRsToTheFirstWorkable(t *testing.T) {
	gh := newFakeGitHub(harnessPR(1, "a1"), harnessPR(2, "b2"))
	gh.checks["b2"] = github.ChecksRed

	item, _, ok, _ := newTestSource(gh, nil).Next()

	if !ok || item.Key != "PR-2" {
		t.Fatalf("Next = %+v ok=%v, want PR-2", item, ok)
	}
}

// A failed list read is not an empty queue: the daemon narrates it as such.
func TestNextSurfacesAListFailure(t *testing.T) {
	gh := newFakeGitHub()
	gh.listErr = errors.New("HTTP 502")

	if _, _, _, err := newTestSource(gh, nil).Next(); err == nil {
		t.Fatal("a failed list must come back as an error")
	}
}

// The plan is read-only: a dry-run that lists an exhausted PR says what the loop
// would do and does none of it.
func TestPlanChangesNothing(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "d4"), harnessPR(13, "e5"))
	gh.merge[12] = ci.MergeConflicting
	gh.checks["e5"] = github.ChecksRed
	gh.comments[13] = []github.PRComment{
		{Body: attemptMarker("a1", CIRed)}, {Body: attemptMarker("b2", CIRed)}, {Body: attemptMarker("c3", CIRed)},
	}

	plan, err := newTestSource(gh, nil).Plan()

	if err != nil || len(plan) != 2 {
		t.Fatalf("Plan = %+v, %v", plan, err)
	}
	if plan[0].Skip != "" || plan[0].Kind != Conflict || !plan[1].Exhausted {
		t.Errorf("plan = %+v, want PR 12 workable and PR 13 exhausted", plan)
	}
	if len(gh.added)+len(gh.posted)+len(gh.removed) != 0 {
		t.Errorf("a plan must change nothing; added %v posted %v removed %v", gh.added, gh.posted, gh.removed)
	}
	out := RenderPlan(plan)
	for _, want := range []string{"PR #12", "rebase", "force-with-lease", "PR #13", "blocked"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered plan missing %q:\n%s", want, out)
		}
	}
}

// The reaper sees the PRs this loop holds, timed by its latest attempt marker, so a
// label stranded by a dead daemon is released past the claim TTL; releasing one
// removes the in-progress label.
func TestClaimsAndReleaseWorkTheHeldLabel(t *testing.T) {
	held := harnessPR(12, "a1")
	held.Labels = []string{"in-progress"}
	gh := newFakeGitHub(held, harnessPR(13, "b2"))
	startedAt := testNow.Add(-2 * time.Hour)
	gh.comments[12] = []github.PRComment{{Body: attemptMarker("a1", Conflict), CreatedAt: startedAt}}
	s := newTestSource(gh, nil)

	claims, err := s.Claims()
	if err != nil || len(claims) != 1 || claims[0].Key != "PR-12" || !claims[0].StartedAt.Equal(startedAt) || claims[0].HasLinkedPR {
		t.Fatalf("Claims = %+v, %v; want PR-12 started at its latest attempt", claims, err)
	}
	if s.HasRemoteBranch("PR-12") {
		t.Error("a PR's branch always exists; it must not spare a stale PR lease")
	}
	if err := s.Release("PR-12"); err != nil || strings.Join(gh.removed[12], ",") != "in-progress" {
		t.Errorf("Release = %v, removed %v; want the in-progress label removed", err, gh.removed[12])
	}
}

// The Source's knobs come from the Consumer's config: its branch prefix, the PR
// loop's opt-in label and budget, and — when its tracker is GitHub Issues — the
// label names it already uses for held and blocked work, so one repository speaks
// one vocabulary. The built-in pr-doctor loop takes the label and budget from the
// PR_LOOP_* env knobs.
func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Config{
		Host:    config.Host{PRLoopLabel: "bot-fix", PRLoopMaxAttempts: 5},
		Project: config.Project{BranchPrefix: "fix"},
	}
	prDoctor, _ := cfg.LoopNamed(config.PRDoctorLoopName)
	want := Options{BranchPrefix: "fix", OptInLabel: "bot-fix", HeldLabel: "in-progress", BlockedLabel: "blocked", MaxAttempts: 5, BaseBranch: "main", LogDir: ".agent-harness/logs/loops/pr-doctor"}
	if got := OptionsFrom(cfg, prDoctor); got != want {
		t.Errorf("OptionsFrom = %+v, want %+v", got, want)
	}

	cfg.Tracker = config.TrackerConfig{Kind: "github", InProgressLabel: "wip", BlockedLabel: "on-hold"}
	if got := OptionsFrom(cfg, prDoctor); got.HeldLabel != "wip" || got.BlockedLabel != "on-hold" {
		t.Errorf("OptionsFrom = %+v, want the GitHub tracker's wip / on-hold labels", got)
	}
}

// A declared loop's source parameters override the env knobs: its own opt-in
// label, its own budget, and which failure kinds it is for.
func TestOptionsFromADeclaredLoop(t *testing.T) {
	cfg := config.Config{
		Host:    config.Host{PRLoopLabel: "bot-fix", PRLoopMaxAttempts: 5},
		Project: config.Project{BranchPrefix: "feat"},
	}
	l := config.Loop{Name: "conflicts", Source: config.LoopSource{Kind: config.SourceGitHubPRs, Label: "rebase-me", When: []string{config.WhenConflicting}, MaxAttempts: 1}}
	got := OptionsFrom(cfg, l)
	if got.OptInLabel != "rebase-me" || got.MaxAttempts != 1 || got.Only != Conflict || got.LogDir != ".agent-harness/logs/loops/conflicts" {
		t.Errorf("OptionsFrom = %+v", got)
	}
}

// A loop for one failure kind skips a PR failing the other way: it is another
// loop's work, or no loop's.
func TestNextYieldsOnlyTheKindTheLoopIsFor(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "abc1234"), harnessPR(13, "def5678"))
	gh.checks["abc1234"] = github.ChecksRed
	gh.merge[13] = ci.MergeConflicting
	opts := testOptions
	opts.Only = Conflict
	s := NewSource(gh, opts, nil, &quietLog{})

	item, _, ok, err := s.Next()

	if err != nil || !ok || item.Key != "PR-13" {
		t.Fatalf("Next = %+v ok=%v err=%v, want the conflicting PR-13", item, ok, err)
	}
	plan, _ := s.Plan()
	if !strings.Contains(plan[0].Skip, "not this loop's") {
		t.Errorf("PR-12's skip = %q", plan[0].Skip)
	}
	if len(gh.posted[12]) != 0 || len(gh.added[12]) != 0 {
		t.Error("a PR of the other kind must not be touched")
	}
}
