package prloop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/stages"
)

// A checkout whose origin/main carries a minimal harness config, with the
// environment [stages.LoadConfig] reads pointed at it, so a Pipeline's Run gets
// as far as its fix.
func configuredCheckout(t *testing.T) {
	t.Helper()
	repo := gittest.New(t)
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(repo.Dir, ".agent-harness", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", "image = \"img:latest\"\n\n[tracker]\nkind = \"github\"\nrepo = \"owner/name\"\n\n[[gates]]\nname = \"check\"\ncommand = \"make check\"\n")
	for _, stage := range []string{"implement", "review", "retro"} {
		write(filepath.Join("prompts", stage+".md"), "/"+stage)
	}
	repo.Git(repo.Dir, "add", "-A")
	repo.Git(repo.Dir, "commit", "-q", "-m", "add the agent-harness config")
	repo.Git(repo.Dir, "push", "-q", "origin", "main")
	home := t.TempDir()
	t.Setenv("HARNESS_ENV_FILE", filepath.Join(home, "none.env"))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("PROJECT_PATH", repo.Dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-x")
	t.Setenv("GH_TOKEN", "ghp_x")
}

// #416: a PR-loop run's fix, its retrospective included, runs under the run lock
// for the PR's key, so the merged-worktree prune leaves its worktree. A run whose
// lock another live run holds runs no fix and comes back RunLive.
func TestRunFixesThePRUnderItsRunLock(t *testing.T) {
	configuredCheckout(t)
	var keys []string
	p := Pipeline{Loop: prDoctor(), RunLock: func(key, runID string, run func() stages.Result) stages.Result {
		keys = append(keys, key)
		if runID == "" {
			t.Error("the run lock was taken with no run id")
		}
		return stages.Result{Disposition: stages.RunLive}
	}}
	item := loop.WorkItem{Key: Key(416), Branch: "feat/gh-416", Context: Item{}}
	got := p.Run(item, &lease.Lease{})
	if got.Disposition != stages.RunLive || len(keys) != 1 || keys[0] != "PR-416" {
		t.Errorf("Run = %+v, lock taken for %v; want the fix run only under PR-416's run lock, and its RunLive returned", got, keys)
	}
}

// #466: a run whose checks poll GitHub refused hands the PR back to its Source,
// which takes the attempt back on the PR, before the lease is released.
func TestRunRequeuesAPRWhoseChecksPollWasRefused(t *testing.T) {
	configuredCheckout(t)
	gh := newFakeGitHub(harnessPR(1466, "7c2d9e1"))
	p := Pipeline{
		Loop:   prDoctor(),
		Source: NewSource(gh, testOptions, nil, &quietLog{}),
		RunLock: func(string, string, func() stages.Result) stages.Result {
			return stages.Result{Disposition: stages.CIAuthFailed}
		},
	}
	it := Item{PR: harnessPR(1466, "7c2d9e1"), Kind: CIRed}
	got := p.Run(loop.WorkItem{Key: Key(1466), Branch: it.PR.HeadBranch, Context: it}, &lease.Lease{})
	if got.Disposition != stages.CIAuthFailed {
		t.Fatalf("Run = %+v, want CIAuthFailed", got)
	}
	if posted := strings.Join(gh.posted[1466], "\n"); !strings.Contains(posted, "void sha=7c2d9e1") {
		t.Errorf("the attempt must be taken back on the PR; posted %q", posted)
	}
}
