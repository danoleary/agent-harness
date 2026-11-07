package prloop

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/loop"
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

// With scope all, a PR off the branch prefix and with no label is worked as a
// human's branch: main is merged in, never rebased. A fork is still skipped.
func TestScopeAllWorksAPRThatDidNotOptIn(t *testing.T) {
	pr := github.PullRequest{Number: 7, HeadBranch: "dependabot/npm/web", HeadSHA: "a1", BaseBranch: "main"}
	gh := newFakeGitHub(pr)
	gh.checks["a1"] = github.ChecksRed
	opts := testOptions
	opts.AllPRs = true
	s := NewSource(gh, opts, nil, &quietLog{})

	item, _, ok, err := s.Next()
	if err != nil || !ok {
		t.Fatalf("Next = ok %v err %v, want the unlabelled PR", ok, err)
	}
	it := item.Context.(Item)
	if it.Harness || it.Strategy().String() != "merge" {
		t.Errorf("item = %+v strategy %v, want a non-harness item merged", it, it.Strategy())
	}

	fork := pr
	fork.Fork = true
	s = NewSource(newFakeGitHub(fork), opts, nil, &quietLog{})
	if plan, _ := s.Plan(); len(plan) != 1 || !strings.Contains(plan[0].Skip, "fork") {
		t.Errorf("plan = %+v, want a fork skipped under scope all", plan)
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

// herd PR #1312 (#167): an attempt at head 9ec4875 pushed nothing (its gate went
// red), so the head did not move. With budget left the PR is retried at once —
// the attempt counts — instead of sitting red waiting for a push nothing makes.
func TestNextRetriesAHeadWhoseAttemptPushedNothing(t *testing.T) {
	pr := harnessPR(1312, "9ec4875")
	pr.HeadBranch = "feat/beh-1234"
	gh := newFakeGitHub(pr)
	gh.checks["9ec4875"] = github.ChecksRed
	gh.comments[1312] = []github.PRComment{{Body: attemptMarker("9ec4875", CIRed)}}
	s := newTestSource(gh, nil)
	plan, _ := s.Plan()
	if out := RenderPlan(plan); !strings.Contains(out, "work: red CI (attempt 2, again: the previous attempt at this head pushed nothing)") {
		t.Errorf("the dry-run must say the PR is retried, and why:\n%s", out)
	}

	item, _, ok, err := s.Next()

	if !ok || err != nil {
		t.Fatalf("Next = ok %v err %v; a red PR with budget left must be retried", ok, err)
	}
	if item.Key != "PR-1312" || item.Branch != "feat/beh-1234" {
		t.Errorf("item = %+v", item)
	}
	if len(gh.posted[1312]) != 1 || !strings.Contains(gh.posted[1312][0], "attempt 2 of 3") || !strings.Contains(gh.posted[1312][0], "pushed nothing") {
		t.Errorf("the retry is recorded as attempt 2, saying why: %v", gh.posted[1312])
	}
}

// The attempt that spends the budget and pushes nothing hands the PR to a human as
// its lease is released, so it is labelled blocked even if the daemon stops before
// its next poll (its breaker counts that attempt too).
func TestReleasingTheLastAttemptHandsTheStillRedPRToAHuman(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "c3"))
	gh.checks["c3"] = github.ChecksRed
	gh.comments[12] = []github.PRComment{{Body: attemptMarker("c3", CIRed)}, {Body: attemptMarker("c3", CIRed)}}

	_, claim, ok, _ := newTestSource(gh, nil).Next()
	if !ok {
		t.Fatal("the last attempt of the budget is worked")
	}
	if err := claim.Release(); err != nil {
		t.Fatal(err)
	}

	if strings.Join(gh.added[12], ",") != "in-progress,blocked" {
		t.Errorf("added = %v, want the lease then blocked", gh.added[12])
	}
	if n := len(gh.posted[12]); n != 2 || !strings.Contains(gh.posted[12][1], "attempt budget") {
		t.Errorf("posted = %v, want the attempt and then the hand-off", gh.posted[12])
	}
}

// Releasing a lease hands nothing off while budget remains, or once the PR is no
// longer failing at the head that was tried.
func TestReleasingALeaseHandsOffOnlyASpentStillRedPR(t *testing.T) {
	cases := []struct {
		name  string
		tried int
		after func(*fakeGitHub)
	}{
		{"budget left", 0, func(*fakeGitHub) {}},
		{"green after the last attempt", 2, func(g *fakeGitHub) { g.checks["c3"] = github.ChecksGreen }},
		{"pushed by the last attempt", 2, func(g *fakeGitHub) { g.prs[0].HeadSHA = "d4" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gh := newFakeGitHub(harnessPR(12, "c3"))
			gh.checks["c3"] = github.ChecksRed
			for i := 0; i < c.tried; i++ {
				gh.comments[12] = append(gh.comments[12], github.PRComment{Body: attemptMarker("c3", CIRed)})
			}
			_, claim, ok, _ := newTestSource(gh, nil).Next()
			if !ok {
				t.Fatal("the PR is worked")
			}
			c.after(gh)
			_ = claim.Release()

			if strings.Contains(strings.Join(gh.added[12], ","), "blocked") || len(gh.posted[12]) != 1 {
				t.Errorf("added %v posted %v, want no hand-off", gh.added[12], gh.posted[12])
			}
		})
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

// #196: a held label with no attempt marker was not set by this loop — a human
// labelled the PR, or another loop holds it under the same label (the security
// loop, during its CI watch). It is not this loop's claim, so the reaper never
// sees it: its zero start time would be older than any TTL and the label stripped.
func TestAHeldPRWithNoAttemptMarkerIsNotAClaim(t *testing.T) {
	held := harnessPR(12, "a1")
	held.Labels = []string{"in-progress"}
	gh := newFakeGitHub(held)
	gh.comments[12] = []github.PRComment{{Body: "a human's review comment", CreatedAt: testNow.Add(-48 * time.Hour)}}
	s := newTestSource(gh, nil)

	claims, err := s.Claims()
	if err != nil || len(claims) != 0 {
		t.Fatalf("Claims = %+v, %v; want none — the loop did not hold PR-12", claims, err)
	}
}

// A held PR whose attempt marker is older than the claim TTL is still a claim: the
// label a dead daemon stranded is still reaped.
func TestAHeldPRWithAnAttemptMarkerPastTheTTLIsStillAClaim(t *testing.T) {
	held := harnessPR(12, "a1")
	held.Labels = []string{"in-progress"}
	gh := newFakeGitHub(held)
	startedAt := testNow.Add(-72 * time.Hour)
	gh.comments[12] = []github.PRComment{{Body: attemptMarker("a1", CIRed), CreatedAt: startedAt}}
	s := newTestSource(gh, nil)

	claims, err := s.Claims()
	if err != nil || len(claims) != 1 || claims[0].Key != "PR-12" || !claims[0].StartedAt.Equal(startedAt) {
		t.Fatalf("Claims = %+v, %v; want PR-12 started at its attempt marker", claims, err)
	}
}

// #173: herd PR-1312's attempt pushed nothing, and the daemon narrated it as
// "PR-1312 produced no PR; releasing back to Todo so it isn't stranded In
// Progress". A PR has no Todo and its run never meant to open a PR: the Source
// words every give-back as what it is, the lease label coming off.
func TestReleasingNarratesTheLeaseLabelNotTodo(t *testing.T) {
	s := newTestSource(newFakeGitHub(), nil)
	for why, want := range map[loop.ReleaseReason]string{
		loop.ReleaseNoPR:     "PR-1312 pushed nothing; releasing its in-progress label",
		loop.ReleaseCapAbort: "spending-cap abort on PR-1312; releasing its in-progress label",
		loop.ReleaseStale:    "reaping stale claim PR-1312 (in-progress label past the claim TTL); releasing the label",
	} {
		if got := s.Releasing(why, "PR-1312"); got != want {
			t.Errorf("Releasing(%d) = %q, want %q", why, got, want)
		}
	}
}

// The Source's knobs come from the Consumer's config: its branch prefix, the PR
// loop's opt-in label and budget, and — when its tracker is GitHub Issues — the
// label names it already uses for held and blocked work, so one repository speaks
// one vocabulary. The built-in pr-doctor loop takes the label and budget from the
// PR_LOOP_* env knobs, and works every open PR.
func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Config{
		Host:    config.Host{PRLoopLabel: "bot-fix", PRLoopMaxAttempts: 5},
		Project: config.Project{BranchPrefix: "fix"},
	}
	prDoctor, _ := cfg.LoopNamed(config.PRDoctorLoopName)
	want := Options{BranchPrefix: "fix", OptInLabel: "bot-fix", AllPRs: true, HeldLabel: "in-progress", BlockedLabel: "blocked", MaxAttempts: 5, BaseBranch: "main", LogDir: ".agent-harness/logs/loops/pr-doctor"}
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
	if got.OptInLabel != "rebase-me" || got.MaxAttempts != 1 || got.Only != Conflict || got.LogDir != ".agent-harness/logs/loops/conflicts" || got.AllPRs {
		t.Errorf("OptionsFrom = %+v", got)
	}

	l.Source.Scope = config.ScopeAll
	if got := OptionsFrom(cfg, l); !got.AllPRs {
		t.Errorf("OptionsFrom = %+v, want AllPRs under scope all", got)
	}

	l.Source.Scope, l.Source.Label = "", ""
	if got := OptionsFrom(cfg, l); !got.AllPRs {
		t.Errorf("OptionsFrom = %+v, want AllPRs by default", got)
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

// fakeWorktrees is the checkout's worktree list the Source reads: each checked-out
// branch and the worktree that holds it.
type fakeWorktrees struct {
	branches map[string]string
	err      error
}

func (f *fakeWorktrees) checkedOut() (map[string]string, error) { return f.branches, f.err }

// ownWorktree is where the PR loop checks PR n out: its own worktree.
func ownWorktree(n int) string { return fmt.Sprintf("/herd/.claude/worktrees/pr-%d", n) }

// herd PR #1375 (#192): feat/beh-1298 conflicted with main while a Claude Code
// worktree had it checked out, so every checkout failed in seconds. The loop spent
// all three attempts between 23:49:01Z and 23:49:30Z and labelled it blocked, and
// no session ever ran. Replayed: the three polls skip it, naming the worktree,
// with no lease, no attempt and no hand-off; the first poll after the worktree is
// removed works it on attempt 1 of 3.
func TestNextSkipsAPRWhoseBranchAnotherWorktreeHasCheckedOut(t *testing.T) {
	pr := harnessPR(1375, "5d0c1a2")
	pr.HeadBranch = "feat/beh-1298"
	gh := newFakeGitHub(pr)
	gh.merge[1375] = ci.MergeConflicting
	wts := &fakeWorktrees{branches: map[string]string{
		"main":          "/herd",
		"feat/beh-1298": "/herd/.claude/worktrees/beh-1298",
	}}
	log := &quietLog{}
	s := NewSource(gh, testOptions, nil, log).WatchWorktrees(wts.checkedOut, ownWorktree)
	s.now = func() time.Time { return testNow }

	for poll := 1; poll <= 3; poll++ {
		if item, _, ok, err := s.Next(); ok || err != nil {
			t.Fatalf("poll %d: Next = %+v ok %v err %v, want the PR skipped", poll, item, ok, err)
		}
	}
	if len(gh.posted[1375])+len(gh.added[1375]) != 0 {
		t.Errorf("a PR another worktree holds must not be touched; posted %v added %v", gh.posted[1375], gh.added[1375])
	}
	plan, _ := s.Plan()
	if want := "branch checked out in worktree /herd/.claude/worktrees/beh-1298"; len(plan) != 1 || plan[0].Skip != want {
		t.Errorf("plan = %+v, want the skip %q", plan, want)
	}
	if out := RenderPlan(plan); !strings.Contains(out, "skip: branch checked out in worktree /herd/.claude/worktrees/beh-1298") {
		t.Errorf("the dry-run must print the skip naming the worktree; got\n%s", out)
	}

	delete(wts.branches, "feat/beh-1298")
	item, _, ok, err := s.Next()
	if err != nil || !ok || item.Key != "PR-1375" {
		t.Fatalf("Next = %+v ok %v err %v, want PR-1375 once the worktree is gone", item, ok, err)
	}
	if len(gh.posted[1375]) != 1 || !strings.Contains(gh.posted[1375][0], "attempt 1 of 3") {
		t.Errorf("posted = %v, want the first attempt of a full budget", gh.posted[1375])
	}
}

// The PR loop's own worktree for the PR is not in the way: the checkout replaces it.
func TestNextWorksAPRWhoseBranchIsInTheLoopsOwnWorktree(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "abc1234"))
	gh.merge[12] = ci.MergeConflicting
	wts := &fakeWorktrees{branches: map[string]string{"feat/gh-12": ownWorktree(12) + "/"}}
	s := NewSource(gh, testOptions, nil, &quietLog{}).WatchWorktrees(wts.checkedOut, ownWorktree)

	if item, _, ok, err := s.Next(); err != nil || !ok || item.Key != "PR-12" {
		t.Fatalf("Next = %+v ok %v err %v, want PR-12 worked over its own worktree", item, ok, err)
	}
}

// Not knowing which worktrees hold which branches fails the poll, as not knowing
// the ticket loop's holds does: a guess would spend an attempt on a doomed checkout.
func TestPlanFailsWhenTheWorktreesCannotBeListed(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "abc1234"))
	gh.merge[12] = ci.MergeConflicting
	wts := &fakeWorktrees{err: errors.New("not a git repository")}
	s := NewSource(gh, testOptions, nil, &quietLog{}).WatchWorktrees(wts.checkedOut, ownWorktree)

	if _, _, ok, err := s.Next(); ok || err == nil {
		t.Fatalf("Next = ok %v err %v, want the poll to fail", ok, err)
	}
	if len(gh.posted[12])+len(gh.added[12]) != 0 {
		t.Errorf("a failed poll must touch nothing; posted %v added %v", gh.posted[12], gh.added[12])
	}
}

// errDirty is git refusing to drop the dirty worktree herd PR-1384's first attempt
// left (#247), as CheckoutRemote reports it.
var errDirty = &gitpkg.LocalStateError{
	Path: "/herd/.claude/worktrees/pr-1384",
	Op:   "remove the previous worktree",
	Err:  errors.New("exit status 128: fatal: '/herd/.claude/worktrees/pr-1384' contains modified or untracked files, use --force to delete it"),
}

// #247, replaying herd PR-1384 on 2026-11-07: one real fix attempt left the
// worktree dirty, and the next two selections each spent an attempt on a checkout
// git refused in seconds. A refused checkout takes its attempt back and hands the
// PR to a human, naming the PR, the worktree and git's error, so the loop does not
// select it into the same refusal again. Once a human removes the label, the PR
// resumes on the attempt after the one real try.
func TestWithdrawTakesBackTheAttemptAndHandsThePRToAHuman(t *testing.T) {
	pr := harnessPR(1384, "4e1f0c9")
	pr.HeadBranch = "feat/beh-1301"
	gh := newFakeGitHub(pr)
	gh.checks["4e1f0c9"] = github.ChecksRed
	gh.comments[1384] = []github.PRComment{{Body: attemptMarker("4e1f0c9", CIRed)}}
	log := &quietLog{}
	s := NewSource(gh, testOptions, nil, log)
	s.now = func() time.Time { return testNow }

	item, claim, ok, err := s.Next()
	if !ok || err != nil {
		t.Fatalf("Next = ok %v err %v, want PR-1384 on attempt 2", ok, err)
	}
	s.Withdraw(item.Context.(Item), errDirty)
	if err := claim.Release(); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(strings.Join(gh.added[1384], ","), "blocked") {
		t.Errorf("added = %v, want the PR labelled blocked for a human", gh.added[1384])
	}
	posted := strings.Join(gh.posted[1384], "\n")
	if strings.Contains(posted, "attempt budget") || !strings.Contains(posted, errDirty.Path) || !strings.Contains(posted, "does not count as an attempt") {
		t.Errorf("the PR must say the checkout was refused, where, and that no attempt was spent:\n%s", posted)
	}
	streamed := strings.Join(log.events, "\n")
	for _, want := range []string{"PR-1384", errDirty.Path, "contains modified or untracked files"} {
		if !strings.Contains(streamed, want) {
			t.Errorf("the stream must name %q; events:\n%s", want, streamed)
		}
	}
	gh.prs[0].Labels = []string{"blocked"} // the label the hand-off added
	if _, _, ok, _ := s.Next(); ok {
		t.Fatal("a PR handed to a human must not be selected again")
	}

	gh.prs[0].Labels = nil // a human cleaned the worktree up and removed the label
	if _, _, ok, _ := s.Next(); !ok {
		t.Fatal("once the label is gone the PR is worked again")
	}
	last := gh.posted[1384][len(gh.posted[1384])-1]
	if !strings.Contains(last, "attempt 2 of 3") {
		t.Errorf("the refused checkout must not have spent an attempt; posted %q", last)
	}
}

// Three refused checkouts in a row spend nothing: the PR keeps its whole budget
// and is never handed off for using it up.
func TestRefusedCheckoutsSpendNoAttempts(t *testing.T) {
	gh := newFakeGitHub(harnessPR(12, "abc1234"))
	gh.merge[12] = ci.MergeConflicting
	s := newTestSource(gh, nil)

	for try := 1; try <= 3; try++ {
		item, claim, ok, err := s.Next()
		if !ok || err != nil {
			t.Fatalf("try %d: Next = ok %v err %v", try, ok, err)
		}
		s.Withdraw(item.Context.(Item), errDirty)
		_ = claim.Release()
	}

	plan, _ := s.Plan()
	if len(plan) != 1 || plan[0].Attempts != 0 || plan[0].Exhausted || len(plan[0].Tried) != 0 {
		t.Errorf("plan = %+v, want the full budget untouched", plan)
	}
}
