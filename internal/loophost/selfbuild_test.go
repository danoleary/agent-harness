package loophost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/version"
)

// #461, through the Daemon: the harness working on its own repo with a binary
// built before a harness fix merged warns at startup, once the fetch shows main
// has that fix, and a rebuild from main's tip lacks nothing.
func TestStaleBuildWarnsWhenTheHarnessRepoHasHarnessCommitsTheBinaryLacks(t *testing.T) {
	defer func(c string) { version.Commit = c }(version.Commit)
	repo := gittest.New(t)
	if err := os.WriteFile(filepath.Join(repo.Dir, "go.mod"), []byte("module github.com/danoleary/agent-harness\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = repo.Dir
	h := hostFor(t, cfg)
	built, err := h.MainSHA()
	if err != nil {
		t.Fatal(err)
	}
	version.Commit = built

	repo.Merge("internal/pipeline/nowork.go", "package pipeline\n", "Keep passing over a resolved-on-main ticket (#453)")
	if err := h.FetchMain(); err != nil {
		t.Fatal(err)
	}
	if got := h.StaleBuild(); !strings.Contains(got, built[:12]) || !strings.Contains(got, "lacks 1 harness commit") {
		t.Errorf("StaleBuild() = %q; want the build commit and the one harness commit it lacks", got)
	}

	if version.Commit, err = h.MainSHA(); err != nil {
		t.Fatal(err)
	}
	if got := h.StaleBuild(); got != "" {
		t.Errorf("StaleBuild() = %q from main's tip; want no warning", got)
	}
}
