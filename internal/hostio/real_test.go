package hostio

import (
	"path/filepath"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/runlog"
)

func testReal(t *testing.T, cfg config.Config) *Real {
	t.Helper()
	log, err := runlog.New(t.TempDir(), "PROJ-1")
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return New(cfg, log, "20260920-101500", false)
}

// The adapter binds the checkout path and the branch prefix once and mints one
// [git.Worktree] per slug, so a Stage names the ticket and nothing else — the
// checkout, the branch and the worktree path never travel as three loose strings.
func TestRealBindsTheCheckoutAndBranchPrefix(t *testing.T) {
	h := testReal(t, config.Config{ProjectPath: "/Users/dan/my-project", BranchPrefix: "feat"})

	if got, want := h.BranchName("proj-7"), "feat/proj-7"; got != want {
		t.Errorf("BranchName = %q, want %q", got, want)
	}
	if got := h.WorktreePath("proj-7"); !filepath.IsAbs(got) || filepath.Base(got) != "proj-7" {
		t.Errorf("WorktreePath = %q, want an absolute path ending in the slug", got)
	}
}

// BEH-641: a Consumer that declares no source_roots gets no resolved-symbol
// advisory rather than a scan of a guessed directory.
func TestRealSourceRootsAreConsumerDeclaredAndAbsolute(t *testing.T) {
	none := testReal(t, config.Config{ProjectPath: "/p"})
	if got := none.sourceRoots(); got != nil {
		t.Errorf("sourceRoots = %v, want nil when the Consumer declares none (BEH-641)", got)
	}

	declared := testReal(t, config.Config{ProjectPath: "/p", SourceRoots: []string{"src", "  ", "web/src"}})
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
	if up := testReal(t, config.Config{Feedback: config.FeedbackConfig{Upstream: "off"}}).upstream(); up != nil {
		t.Errorf("upstream(off) = %+v, want nil (local sink)", up)
	}
	if up := testReal(t, config.Config{}).upstream(); up != nil {
		t.Errorf("upstream(zero) = %+v, want nil", up)
	}
}

func TestUpstreamGitHubBuildsRepoBoundSink(t *testing.T) {
	up := testReal(t, config.Config{
		GitHubToken: "gh-token",
		Feedback: config.FeedbackConfig{
			Upstream: "github", Repo: "example-org/agent-harness",
			FindingsLabel: "harness-finding", Project: "herd",
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
	up := testReal(t, config.Config{Feedback: config.FeedbackConfig{Upstream: "github", Repo: "not-a-repo"}}).upstream()
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
	if m := testReal(t, config.Config{AnthropicAPIKey: "sk-ant-api03-x"}).matcher(); m == nil {
		t.Error("an API key must enable the semantic dedup pass (BEH-573)")
	}
}

// The tracker is resolved once per run and cached — a stage that fetches the
// ticket and later files findings builds one client, not three.
func TestTrackerIsResolvedOnceAndCached(t *testing.T) {
	h := testReal(t, config.Config{Tracker: config.TrackerConfig{Kind: "nonesuch"}})

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
// signal it is about.
func TestNewFakeIsAHealthyHost(t *testing.T) {
	f := NewFake()

	if err := f.Preflight(); err != nil {
		t.Errorf("Preflight = %v, want a launchable host", err)
	}
	if !f.WorktreeExists("x") || !f.WorktreeClean("x") || f.BranchDiffEmpty("x") {
		t.Error("the default worktree is present, clean, and carries a diff")
	}
	if got := f.Agent(AgentRun{Label: "review"}); got.ExitCode != 0 || !got.ReviewVerdictEmitted {
		t.Errorf("default agent outcome = %+v, want a clean session that reached its verdict", got)
	}
	if _, err := f.Tracker(); err != nil {
		t.Errorf("Tracker = %v, want the scripted tracker", err)
	}
}
