package secloop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
)

// fakeGitHub is the scripted GitHub the Source polls: the open alerts, every PR
// per branch, and the open PRs. Label removals are recorded, so a test asserts the
// lease coming off a PR.
type fakeGitHub struct {
	alerts   []github.Alert
	alertErr error
	branches map[string][]github.BranchPR
	open     []github.PullRequest

	branchReads []string
	removed     map[int][]string
}

func newFakeGitHub(alerts ...github.Alert) *fakeGitHub {
	return &fakeGitHub{alerts: alerts, branches: map[string][]github.BranchPR{}, removed: map[int][]string{}}
}

func (f *fakeGitHub) DependabotAlerts([]string) ([]github.Alert, error) { return f.alerts, f.alertErr }

func (f *fakeGitHub) PRsForBranch(branch string) ([]github.BranchPR, error) {
	f.branchReads = append(f.branchReads, branch)
	return f.branches[branch], nil
}

func (f *fakeGitHub) OpenPRs() ([]github.PullRequest, error) { return f.open, nil }

func (f *fakeGitHub) RemoveLabel(n int, label string) error {
	f.removed[n] = append(f.removed[n], label)
	return nil
}

type recordingLog struct{ records []loopstream.Record }

func (l *recordingLog) Event(s string) { l.records = append(l.records, loopstream.Record{Message: s}) }
func (l *recordingLog) Structured(r loopstream.Record) {
	l.records = append(l.records, r)
}

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

const testBase = "base000111222"

type fixture struct {
	gh   *fakeGitHub
	log  *recordingLog
	dir  string
	base string
	src  *Source
}

func newFixture(t *testing.T, alerts ...github.Alert) *fixture {
	t.Helper()
	f := &fixture{gh: newFakeGitHub(alerts...), log: &recordingLog{}, dir: t.TempDir(), base: testBase}
	opts := Options{BranchPrefix: "feat", HeldLabel: "in-progress", BlockedLabel: "blocked", MaxAttempts: 2, Grace: 24 * time.Hour, LogsDir: f.dir}
	f.src = NewSource(f.gh, opts, func() (string, error) { return f.base, nil }, f.log)
	f.src.now = func() time.Time { return testNow }
	return f
}

func (f *fixture) plan(t *testing.T) []Candidate {
	t.Helper()
	cands, err := f.src.Plan()
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return cands
}

func (f *fixture) only(t *testing.T) Candidate {
	t.Helper()
	cands := f.plan(t)
	if len(cands) != 1 {
		t.Fatalf("Plan = %+v, want one candidate", cands)
	}
	return cands[0]
}

func (f *fixture) ledger(t *testing.T) []Entry {
	t.Helper()
	entries, err := readLedger(filepath.Join(f.dir, "attempts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func npmAlert(n int, pkg, manifest, patched string) github.Alert {
	return github.Alert{Number: n, Ecosystem: "npm", Package: pkg, ManifestPath: manifest, VulnerableRange: "< 9.9.9", Patched: patched}
}

func lodash(n int) github.Alert { return npmAlert(n, "lodash", "pnpm-lock.yaml", "4.17.21") }

// One Work item per (ecosystem, package): alerts on the same package from two
// manifests are one group, keyed SEC-<ecosystem>-<package> on a branch slugged from
// that Key, and a scoped package slugs to a valid branch.
func TestPlanGroupsAlertsPerPackageAcrossManifests(t *testing.T) {
	f := newFixture(t,
		lodash(7),
		npmAlert(3, "lodash", "packages/web/package.json", "4.17.20"),
		npmAlert(5, "@babel/core", "pnpm-lock.yaml", "7.26.10"),
	)

	cands := f.plan(t)

	if len(cands) != 2 {
		t.Fatalf("Plan = %+v, want two groups", cands)
	}
	babel, lo := cands[0], cands[1]
	if babel.Key() != "SEC-npm-@babel/core" || babel.Branch != "feat/sec-npm-babel-core" {
		t.Errorf("babel = key %q branch %q", babel.Key(), babel.Branch)
	}
	if lo.Key() != "SEC-npm-lodash" || lo.Branch != "feat/sec-npm-lodash" {
		t.Errorf("lodash = key %q branch %q", lo.Key(), lo.Branch)
	}
	if len(lo.Alerts) != 2 || lo.Alerts[0].Number != 3 || lo.Alerts[1].Number != 7 {
		t.Errorf("lodash alerts = %+v, want #3 and #7, in number order", lo.Alerts)
	}
	for _, c := range cands {
		if c.Skip != "" || c.Exhausted {
			t.Errorf("%s: skip %q exhausted %v, want it worked", c.Key(), c.Skip, c.Exhausted)
		}
	}
}

// The fingerprint is over the package, the alert numbers and each first patched
// version: order-free, and changed by a new alert or a moved patch.
func TestFingerprint(t *testing.T) {
	a := Fingerprint(Group{Package: "lodash", Alerts: []github.Alert{lodash(7), lodash(3)}})
	b := Fingerprint(Group{Package: "lodash", Alerts: []github.Alert{lodash(3), lodash(7)}})
	if a != b || len(a) != 10 {
		t.Fatalf("fingerprints %q, %q: want one 10-character value regardless of order", a, b)
	}
	if c := Fingerprint(Group{Package: "lodash", Alerts: []github.Alert{lodash(3), lodash(7), lodash(9)}}); c == a {
		t.Error("a new alert must change the fingerprint")
	}
	moved := lodash(7)
	moved.Patched = "4.17.22"
	if c := Fingerprint(Group{Package: "lodash", Alerts: []github.Alert{lodash(3), moved}}); c == a {
		t.Error("a changed patched version must change the fingerprint")
	}
}

// A package with some unpatched alerts is worked for the patched ones; one with
// none patched is skipped (ADR-0040, decision 2).
func TestPlanSkipsAPackageWithNoPatchedVersion(t *testing.T) {
	f := newFixture(t, npmAlert(1, "minimist", "pnpm-lock.yaml", ""), npmAlert(2, "qs", "pnpm-lock.yaml", ""), npmAlert(3, "qs", "pnpm-lock.yaml", "6.11.0"))

	cands := f.plan(t)

	minimist, qs := cands[0], cands[1]
	if !strings.Contains(minimist.Skip, "no patched version") {
		t.Errorf("minimist skip = %q, want no patched version", minimist.Skip)
	}
	if qs.Skip != "" || len(qs.Alerts) != 1 || len(qs.Unpatched) != 1 {
		t.Errorf("qs = %+v, want worked for its one patched alert", qs)
	}
}

// An ecosystem or a lockfile the host check has no reader for is skipped before
// any read, since its fix could not be checked (ADR-0040, decision 4).
func TestPlanSkipsAnUnsupportedEcosystemOrLockfile(t *testing.T) {
	pip := github.Alert{Number: 1, Ecosystem: "pip", Package: "django", ManifestPath: "requirements.txt", Patched: "4.2.1"}
	yarn := npmAlert(2, "axios", "yarn.lock", "1.7.4")
	f := newFixture(t, pip, yarn)

	cands := f.plan(t)

	if !strings.Contains(cands[0].Skip, "yarn.lock") {
		t.Errorf("axios skip = %q, want the unsupported lockfile named", cands[0].Skip)
	}
	if !strings.Contains(cands[1].Skip, "pip") {
		t.Errorf("django skip = %q, want the unsupported ecosystem named", cands[1].Skip)
	}
	if len(f.gh.branchReads) != 0 {
		t.Errorf("read branches %v, want none for groups skipped on cheap checks", f.gh.branchReads)
	}
}

// The PRs on the package's branch: an open one, a blocked label, one a human closed
// at this fingerprint, and one merged at it whose alerts are still open, inside and
// past the grace period.
func TestPlanSkipsOnThePRsOnTheBranch(t *testing.T) {
	fp := Fingerprint(Group{Package: "lodash", Alerts: []github.Alert{lodash(7)}})
	cases := []struct {
		name string
		prs  []github.BranchPR
		want string
	}{
		{"open PR", []github.BranchPR{{Number: 40, State: "open"}}, "open PR #40"},
		{"blocked", []github.BranchPR{{Number: 41, State: "closed", Labels: []string{"blocked"}}}, "labelled blocked"},
		{"closed by a human", []github.BranchPR{{Number: 42, State: "closed", Body: "fix\n" + Marker(fp)}}, "closed"},
		{"merged, in grace", []github.BranchPR{{Number: 43, State: "closed", MergedAt: testNow.Add(-time.Hour), Body: Marker(fp)}}, "waiting"},
		{"merged, past grace", []github.BranchPR{{Number: 44, State: "closed", MergedAt: testNow.Add(-48 * time.Hour), Body: Marker(fp)}}, "still open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, lodash(7))
			f.gh.branches["feat/sec-npm-lodash"] = tc.prs

			c := f.only(t)

			if !strings.Contains(c.Skip, tc.want) {
				t.Errorf("skip = %q, want it to contain %q", c.Skip, tc.want)
			}
			if c.PR == nil || c.PR.Number != tc.prs[0].Number {
				t.Errorf("PR = %+v, want #%d", c.PR, tc.prs[0].Number)
			}
		})
	}
}

// A closed or merged PR at an older fingerprint says nothing about this alert set:
// a new alert is worked afresh.
func TestPlanWorksPastAPRAtAnOlderFingerprint(t *testing.T) {
	f := newFixture(t, lodash(7), lodash(8))
	old := Fingerprint(Group{Package: "lodash", Alerts: []github.Alert{lodash(7)}})
	f.gh.branches["feat/sec-npm-lodash"] = []github.BranchPR{
		{Number: 42, State: "closed", Body: Marker(old)},
		{Number: 44, State: "closed", MergedAt: testNow.Add(-48 * time.Hour), Body: Marker(old)},
	}

	if c := f.only(t); c.Skip != "" {
		t.Errorf("skip = %q, want it worked", c.Skip)
	}
}

// Next holds the lease, which appends a started line to the ledger, and yields the
// item on its branch.
func TestNextHoldsTheLeaseWithAStartedLine(t *testing.T) {
	f := newFixture(t, lodash(7))

	item, claim, ok, err := f.src.Next()

	if err != nil || !ok {
		t.Fatalf("Next = ok %v, err %v", ok, err)
	}
	if item.Key != "SEC-npm-lodash" || item.Branch != "feat/sec-npm-lodash" {
		t.Errorf("item = %+v", item)
	}
	if it, _ := item.Context.(Item); it.Fingerprint == "" || len(it.Alerts) != 1 {
		t.Errorf("context = %+v, want the group and its fingerprint", item.Context)
	}
	if claim == nil || !claim.Held() {
		t.Fatal("want a held lease")
	}
	got := f.ledger(t)
	if len(got) != 1 || got[0].Outcome != OutcomeStarted || got[0].Key != "SEC-npm-lodash" || got[0].BaseSHA != testBase || !got[0].At.Equal(testNow) {
		t.Errorf("ledger = %+v, want one started line at the base", got)
	}
}

// An attempt that pushed nothing at this origin/main is not retried until main
// moves; once it does, the same fingerprint is retried and counts against the
// budget.
func TestPlanSkipsAnUnpushedAttemptWhileTheBaseIsUnchanged(t *testing.T) {
	f := newFixture(t, lodash(7))
	if _, _, ok, _ := f.src.Next(); !ok {
		t.Fatal("first Next yielded nothing")
	}

	if c := f.only(t); !strings.Contains(c.Skip, "origin/main") {
		t.Errorf("skip = %q, want the unchanged base named", c.Skip)
	}

	f.base = "base999"
	if c := f.only(t); c.Skip != "" || c.Attempts != 1 || c.Exhausted {
		t.Errorf("after main moved = %+v, want attempt 2 of 2", c)
	}
}

// Past max_attempts on one fingerprint the package is handed to a human once: a
// handed_off line and a released stream record, then a skip naming the hand-off.
func TestNextHandsOffAnExhaustedFingerprint(t *testing.T) {
	f := newFixture(t, lodash(7))
	for _, base := range []string{"b1", "b2"} {
		f.base = base
		if _, _, ok, err := f.src.Next(); !ok || err != nil {
			t.Fatalf("Next at %s = ok %v err %v", base, ok, err)
		}
	}
	f.base = "b3"
	if c := f.only(t); !c.Exhausted || c.Attempts != 2 || c.Skip != "" {
		t.Fatalf("candidate = %+v, want exhausted after 2 attempts", c)
	}

	_, _, ok, err := f.src.Next()

	if ok || err != nil {
		t.Fatalf("Next = ok %v err %v, want nothing to work", ok, err)
	}
	got := f.ledger(t)
	if last := got[len(got)-1]; last.Outcome != OutcomeHandedOff || last.Key != "SEC-npm-lodash" {
		t.Errorf("last ledger line = %+v, want handed_off", last)
	}
	var released bool
	for _, r := range f.log.records {
		released = released || (r.Kind == loopstream.KindTicketReleased && r.Ticket == "SEC-npm-lodash")
	}
	if !released {
		t.Errorf("records = %+v, want a KindTicketReleased record", f.log.records)
	}
	if c := f.only(t); !strings.Contains(c.Skip, "handed to a human") || c.Exhausted {
		t.Errorf("after hand-off = %+v, want a skip naming the hand-off", c)
	}
	if _, _, _, err := f.src.Next(); err != nil {
		t.Fatal(err)
	}
	if n := len(f.ledger(t)); n != len(got) {
		t.Errorf("ledger grew to %d lines, want the hand-off written once", n)
	}
}

// A new alert changes the fingerprint and starts a fresh budget.
func TestANewFingerprintStartsAFreshBudget(t *testing.T) {
	f := newFixture(t, lodash(7))
	for _, base := range []string{"b1", "b2"} {
		f.base = base
		f.src.Next()
	}
	f.src.Next() // hands off

	f.gh.alerts = append(f.gh.alerts, lodash(8))
	c := f.only(t)

	if c.Skip != "" || c.Exhausted || c.Attempts != 0 {
		t.Errorf("candidate = %+v, want a fresh budget", c)
	}
}

// A failed alert read fails the poll rather than reading as an empty queue.
func TestPlanFailsOnAnAlertReadError(t *testing.T) {
	f := newFixture(t)
	f.gh.alertErr = errors.New("HTTP 403")

	if _, err := f.src.Plan(); err == nil {
		t.Fatal("want the read error")
	}
	if _, _, ok, err := f.src.Next(); ok || err == nil {
		t.Fatalf("Next = ok %v err %v, want the error", ok, err)
	}
}

// A ledger that cannot be read fails the poll: the budget would be unknown.
func TestPlanFailsOnAnUnreadableLedger(t *testing.T) {
	f := newFixture(t, lodash(7))
	if err := os.WriteFile(filepath.Join(f.dir, "attempts.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.src.Plan(); err == nil {
		t.Fatal("want the ledger error")
	}
}

// Once ship opens the PR it carries the held label. Claims lists the labelled PRs
// on sec- branches, timed by the item's latest started line; a labelled PR the
// ledger never started is not this loop's claim. Release takes the label off.
func TestClaimsAndReleaseTheHeldLabel(t *testing.T) {
	f := newFixture(t, lodash(7))
	if _, _, ok, _ := f.src.Next(); !ok {
		t.Fatal("Next yielded nothing")
	}
	f.gh.open = []github.PullRequest{
		{Number: 50, HeadBranch: "feat/sec-npm-lodash", Labels: []string{"in-progress"}},
		{Number: 51, HeadBranch: "feat/sec-npm-qs", Labels: []string{"in-progress"}},
		{Number: 52, HeadBranch: "feat/gh-12", Labels: []string{"in-progress"}},
		{Number: 53, HeadBranch: "feat/sec-npm-axios"},
	}

	claims, err := f.src.Claims()

	if err != nil {
		t.Fatal(err)
	}
	want := []loop.StaleClaim{{Key: "SEC-npm-lodash", StartedAt: testNow}}
	if len(claims) != 1 || claims[0] != want[0] {
		t.Errorf("claims = %+v, want %+v", claims, want)
	}
	if err := f.src.Release("SEC-npm-lodash"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.gh.removed[50], ",") != "in-progress" {
		t.Errorf("removed = %v, want the in-progress label off #50", f.gh.removed)
	}
}

// The run's own lease: releasing it takes the held label off the item's open PR,
// if ship opened one, and touches nothing otherwise.
func TestReleasingTheLeaseTakesTheLabelOff(t *testing.T) {
	f := newFixture(t, lodash(7))
	_, claim, ok, _ := f.src.Next()
	if !ok {
		t.Fatal("Next yielded nothing")
	}
	f.gh.open = []github.PullRequest{{Number: 50, HeadBranch: "feat/sec-npm-lodash", Labels: []string{"in-progress"}}}

	if err := claim.Release(); err != nil {
		t.Fatal(err)
	}
	if claim.Held() || strings.Join(f.gh.removed[50], ",") != "in-progress" {
		t.Errorf("held %v removed %v, want the lease released and the label off", claim.Held(), f.gh.removed)
	}
}

// The daemon's ticket breadcrumbs are not posted, and a branch on the remote says
// nothing about the lease.
func TestCommentIsANoOpAndHasRemoteBranchIsFalse(t *testing.T) {
	f := newFixture(t)
	if err := f.src.Comment("SEC-npm-lodash", "released back to Todo"); err != nil {
		t.Error(err)
	}
	if f.src.HasRemoteBranch("SEC-npm-lodash") {
		t.Error("HasRemoteBranch = true, want false")
	}
}
