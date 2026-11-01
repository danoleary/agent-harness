package fix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
)

// conflictEnv is an Env over a real worktree carrying a handoff, whose gates are
// green and whose conflict-resolution session does resolve.
func conflictEnv(t *testing.T, resolve func(*gittest.Repo, *hostio.Fake)) (Env, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	repo.Handoff("proj-1")
	h := hostio.NewFake(repo.Checkout(), "proj-1")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		resolve(repo, h)
		return session.Outcome{}
	}
	log, err := runlog.New(t.TempDir(), "PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.GateLockDir = t.TempDir()
	return Env{Host: h, Cfg: cfg, Log: log, Who: "review"}, repo
}

func runLog(t *testing.T, e Env) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.Log.Dir, "run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// #164, replayed: the resolution session ran the same replay against the same
// origin/main the branch was cut from. Both commits applied and the tree matched
// the reviewed tip byte for byte. Nothing was resolved, so the log must not say a
// conflict was.
func TestAResolutionWhoseReplayWasCleanIsNotLoggedAsAResolvedConflict(t *testing.T) {
	e, _ := conflictEnv(t, func(repo *gittest.Repo, h *hostio.Fake) {
		wt := h.Worktree()
		tip, _ := wt.HeadSHA()
		repo.Git(wt.Path(), "reset", "-q", "--hard", "origin/main")
		repo.Git(wt.Path(), "cherry-pick", "origin/main.."+tip)
	})

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return GateResult{} }})

	if !res.OK {
		t.Fatalf("a branch on origin/main with a green re-gate is clear to push, got %+v", res)
	}
	got := runLog(t, e)
	if !strings.Contains(got, "replay was clean in the sandbox — no conflict to resolve") {
		t.Errorf("run.jsonl must say the replay was clean; got %s", got)
	}
	if strings.Contains(got, "conflict resolved") {
		t.Errorf("run.jsonl must not claim a conflict was resolved; got %s", got)
	}
}

// A session that resolved a real conflict is still logged as having done so.
func TestAResolvedConflictIsLoggedAsResolved(t *testing.T) {
	e, repo := conflictEnv(t, func(repo *gittest.Repo, h *hostio.Fake) { repo.Resolve(h.Worktree()) })
	repo.Conflict("proj-1")
	if err := e.Host.FetchMain(); err != nil {
		t.Fatal(err)
	}

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return GateResult{} }})

	if !res.OK {
		t.Fatalf("a resolved, re-gated conflict is clear to push, got %+v", res)
	}
	if got := runLog(t, e); !strings.Contains(got, "conflict resolved") || strings.Contains(got, "no conflict to resolve") {
		t.Errorf("run.jsonl must say the conflict was resolved; got %s", got)
	}
}
