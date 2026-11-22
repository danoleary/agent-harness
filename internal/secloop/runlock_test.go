package secloop

import (
	"os"
	"path/filepath"
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

// #416: a security-alerts run's fix, its retrospective included, runs under the
// run lock for the package's key, so the merged-worktree prune leaves its
// worktree. A run whose lock another live run holds runs no fix and comes back
// RunLive.
func TestRunFixesThePackageUnderItsRunLock(t *testing.T) {
	configuredCheckout(t)
	it := Item{Group: Group{Ecosystem: "npm", Package: "lodash"}, Branch: "feat/sec-npm-lodash"}
	var keys []string
	p := Pipeline{Loop: securityLoop(), RunLock: func(key, runID string, run func() stages.Result) stages.Result {
		keys = append(keys, key)
		if runID == "" {
			t.Error("the run lock was taken with no run id")
		}
		return stages.Result{Disposition: stages.RunLive}
	}}
	got := p.Run(loop.WorkItem{Key: it.Key(), Branch: it.Branch, Context: it}, &lease.Lease{})
	if got.Disposition != stages.RunLive || len(keys) != 1 || keys[0] != it.Key() {
		t.Errorf("Run = %+v, lock taken for %v; want the fix run only under %s's run lock, and its RunLive returned", got, keys, it.Key())
	}
}
