package hostio

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
)

func testReal(t *testing.T, cfg config.Config) *Real {
	t.Helper()
	log, err := runlog.New(t.TempDir(), "PROJ-1")
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return New(cfg, log, "20260920-101500", false, "proj-7")
}

// The adapter is bound to one ticket: the checkout path, the branch prefix and the
// slug are fixed when it is built, and it hands out that ticket's [git.Worktree]
// whole, so a Stage never names the ticket again.
func TestRealIsBoundToOneTicketsWorktree(t *testing.T) {
	h := testReal(t, config.Config{Host: config.Host{ProjectPath: "/Users/dan/my-project"}, Project: config.Project{BranchPrefix: "feat"}})

	wt := h.Worktree()
	if got, want := wt.Branch(), "feat/proj-7"; got != want {
		t.Errorf("Branch = %q, want %q", got, want)
	}
	if got := wt.Path(); !filepath.IsAbs(got) || filepath.Base(got) != "proj-7" {
		t.Errorf("Path = %q, want an absolute path ending in the slug", got)
	}
}

// BEH-641: a Consumer that declares no source_roots gets no resolved-symbol
// advisory rather than a scan of a guessed directory.
func TestRealSourceRootsAreConsumerDeclaredAndAbsolute(t *testing.T) {
	none := testReal(t, config.Config{Host: config.Host{ProjectPath: "/p"}})
	if got := none.sourceRoots(); got != nil {
		t.Errorf("sourceRoots = %v, want nil when the Consumer declares none (BEH-641)", got)
	}

	declared := testReal(t, config.Config{Host: config.Host{ProjectPath: "/p"}, Project: config.Project{SourceRoots: []string{"src", "  ", "web/src"}}})
	want := []string{filepath.Join("/p", "src"), filepath.Join("/p", "web/src")}
	got := declared.sourceRoots()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sourceRoots = %v, want %v (blank entries dropped, resolved against the checkout)", got, want)
	}
}

// BEH-640/ADR-0011: the opt-in public-harness-repo sink is built from config.
// off (the default) yields a nil sink so harness findings stay local; github
// yields a sink bound to the configured public repo, carrying the reporting
// project name for provenance. It lives here, not in internal/stages, which is
// what lets the stage layer stop importing a concrete tracker adapter.
func TestUpstreamOffYieldsNilSink(t *testing.T) {
	if up := testReal(t, config.Config{Project: config.Project{Feedback: config.FeedbackConfig{Upstream: "off"}}}).upstream(); up != nil {
		t.Errorf("upstream(off) = %+v, want nil (local sink)", up)
	}
	if up := testReal(t, config.Config{}).upstream(); up != nil {
		t.Errorf("upstream(zero) = %+v, want nil", up)
	}
}

func TestUpstreamGitHubBuildsRepoBoundSink(t *testing.T) {
	up := testReal(t, config.Config{
		Host: config.Host{
			GitHubToken: "gh-token",
		},
		Project: config.Project{
			Feedback: config.FeedbackConfig{
				Upstream: "github", Repo: "example-org/agent-harness",
				FindingsLabel: "harness-finding", Project: "herd",
			},
		},
	}).upstream()

	if up == nil {
		t.Fatal("upstream(github) = nil, want a sink")
	}
	if up.Container != "example-org/agent-harness" {
		t.Errorf("Container = %q, want the upstream repo slug", up.Container)
	}
	if up.Project != "herd" {
		t.Errorf("Project = %q, want the reporting-project name", up.Project)
	}
	if up.Sink == nil {
		t.Errorf("upstream must wire a findings sink, got %+v", up)
	}
}

// A malformed repo never reaches here (config validates the shape), and
// trackers.New rejects it again; that defensive failure degrades to the local
// sink rather than filing nowhere.
func TestUpstreamMalformedRepoDegradesToTheLocalSink(t *testing.T) {
	up := testReal(t, config.Config{Project: config.Project{Feedback: config.FeedbackConfig{Upstream: "github", Repo: "not-a-repo"}}}).upstream()
	if up != nil {
		t.Errorf("upstream = %+v, want nil so harness findings stay local", up)
	}
}

// BEH-316/BEH-573: the semantic dedup pass needs an `sk-ant-api03-` API key; a
// subscription OAuth token is rejected by the x-api-key header. With no key the
// matcher must be a TRUE nil interface so filing's nil check fires and it degrades
// to exact key/title dedup.
func TestMatcherIsNilWithoutAnAnthropicAPIKey(t *testing.T) {
	if m := testReal(t, config.Config{}).matcher(); m != nil {
		t.Errorf("matcher = %v, want a true nil interface so filing skips the semantic pass", m)
	}
	if m := testReal(t, config.Config{Host: config.Host{AnthropicAPIKey: "sk-ant-api03-x"}}).matcher(); m == nil {
		t.Error("an API key must enable the semantic dedup pass (BEH-573)")
	}
}

// The tracker is resolved once per run and cached — a stage that fetches the
// ticket and later files findings builds one client, not three.
func TestTrackerIsResolvedOnceAndCached(t *testing.T) {
	h := testReal(t, config.Config{Project: config.Project{Tracker: config.TrackerConfig{Kind: "nonesuch"}}})

	_, first := h.Tracker()
	_, second := h.Tracker()

	if first == nil {
		t.Fatal("an unknown tracker must be an error")
	}
	if first != second {
		t.Errorf("the resolution (and its error) must be cached: %v != %v", first, second)
	}
}

// NewFake is a healthy host on the happy path, so a test overrides only the one
// signal it is about. Its git is not scripted: a fresh repo has no worktree for the
// ticket, and the Fake says so.
func TestNewFakeIsAHealthyHost(t *testing.T) {
	f := NewFake(gittest.New(t).Checkout(), "proj-1")

	if err := f.Preflight(); err != nil {
		t.Errorf("Preflight = %v, want a launchable host", err)
	}
	if f.Worktree().Exists() {
		t.Error("nothing created the ticket's worktree, so it does not exist")
	}
	if got := f.Agent(AgentRun{Label: "review"}); got.ExitCode != 0 || !got.ReviewVerdictEmitted {
		t.Errorf("default agent outcome = %+v, want a clean session that reached its verdict", got)
	}
	if _, err := f.Tracker(); err != nil {
		t.Errorf("Tracker = %v, want the scripted tracker", err)
	}
}

// #28: the Fake's git state is git's. A session that commits inside AgentFn moves
// the branch the Stage reads next, and a push the Fake lets through reaches origin
// — state changes the boolean flags could only fake with a per-test subclass.
func TestFakeGitStateChangesWithTheRepository(t *testing.T) {
	repo := gittest.New(t)
	f := NewFake(repo.Checkout(), "proj-1")
	wt := repo.Handoff("proj-1")
	f.AgentFn = func(int, AgentRun) session.Outcome {
		repo.Commit(wt, "more.txt", "more\n", "feat: and more")
		return session.Outcome{}
	}

	if got := f.Worktree().CommitsAhead(); got != 1 {
		t.Fatalf("CommitsAhead = %d, want the one handoff commit", got)
	}
	f.Agent(AgentRun{Label: "implementation"})
	if got := f.Worktree().CommitsAhead(); got != 2 {
		t.Errorf("CommitsAhead = %d after a session committed, want 2", got)
	}
	if f.Worktree().BranchPushed() {
		t.Fatal("nothing pushed the branch yet")
	}
	if err := f.PushForceWithLease(); err != nil {
		t.Fatalf("PushForceWithLease: %v", err)
	}
	if !f.Worktree().BranchPushed() || repo.OnOrigin("feat/proj-1") == "" {
		t.Error("a push the Fake lets through must reach origin")
	}
}

// The remote half stays scripted: a push the test fails never reaches origin.
func TestFakeScriptedPushFailureReachesNothing(t *testing.T) {
	repo := gittest.New(t)
	repo.Handoff("proj-1")
	f := NewFake(repo.Checkout(), "proj-1")
	f.ForcePushErr = errors.New("remote rejected")

	if err := f.PushForceWithLease(); err == nil {
		t.Fatal("the scripted failure must surface")
	}
	if repo.OnOrigin("feat/proj-1") != "" {
		t.Error("a failed push must leave origin untouched")
	}
}

// ADR-0019: the PR loop's Host binds its slug to the PR's head branch, which the
// harness did not name; the worktree path stays slug-keyed.
func TestNewOnBranchBindsOneSlugToTheNamedBranch(t *testing.T) {
	log, err := runlog.New(t.TempDir(), "PR-12")
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	cfg := config.Config{Host: config.Host{ProjectPath: "/p"}, Project: config.Project{BranchPrefix: "feat"}}
	h := NewOnBranch(cfg, log, "20260920-101500", false, "pr-12", "alice/widget")

	if got := h.Worktree().Branch(); got != "alice/widget" {
		t.Errorf("Branch = %q, want the PR's head branch", got)
	}
	if got := filepath.Base(h.Worktree().Path()); got != "pr-12" {
		t.Errorf("Path ends in %q, want the slug", got)
	}
}
