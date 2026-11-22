package fix

import (
	"errors"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// Env.CI is the CIFix callback a CI watch runs over a red check: a sandboxed
// diagnose-and-fix session, then ground-truth enforcement. The watch itself is
// host-side (ADR-0002), so these tests call the callback the way the watch would.

// The clean case: the fix session exits 0 over a clean worktree having found no
// code defect, and the harness adds a re-trigger commit so CI re-runs against a
// fresh HEAD (BEH-561).
func TestACIFixOverACleanWorktreeAddsTheReTriggerCommit(t *testing.T) {
	e, repo := conflictEnv(t, func(*gittest.Repo, *hostio.Fake) {})
	h := e.Host.(*hostio.Fake)

	if err := e.CI(Target{Slug: "proj-1"})("FAIL TestThing", true); err != nil {
		t.Fatalf("a fix session over a clean worktree is accepted, got %v", err)
	}
	if len(h.Agents) != 1 || h.Agents[0].Label != "cifix-1" {
		t.Errorf("agent runs = %+v, want one auto-fix session labelled per attempt", h.Agents)
	}
	if want := strings.SplitN(gitpkg.CIRerunMessage(), "\n", 2)[0]; repo.Subject(h.Worktree()) != want {
		t.Errorf("HEAD is %q, want the re-trigger commit so CI re-runs against a fresh HEAD (BEH-561)", repo.Subject(h.Worktree()))
	}
}

// Ground truth, never the agent's say-so: a fix session that left the worktree
// dirty has nothing trustworthy to push.
func TestACIFixThatLeftTheWorktreeDirtyIsRefused(t *testing.T) {
	e, repo := conflictEnv(t, func(repo *gittest.Repo, h *hostio.Fake) { repo.Dirty(h.Worktree()) })
	h := e.Host.(*hostio.Fake)

	err := e.CI(Target{Slug: "proj-1"})("FAIL TestThing", true)

	if err == nil || !strings.Contains(err.Error(), "left uncommitted changes") {
		t.Fatalf("an uncommitted fix must be refused, got %v", err)
	}
	if repo.Subject(h.Worktree()) != gittest.HandoffSubject {
		t.Errorf("no re-trigger commit may follow a refused fix; HEAD is %q", repo.Subject(h.Worktree()))
	}
}

// BEH-571: a fix session aborted by a spending cap is its own retry-after-reset
// class, so the watch defers rather than counting a 1-second no-op as a failure.
func TestACIFixCutShortByTheSpendingCapIsItsOwnClass(t *testing.T) {
	e, _ := conflictEnv(t, func(*gittest.Repo, *hostio.Fake) {})
	e.Host.(*hostio.Fake).AgentFn = func(int, hostio.AgentRun) session.Outcome {
		return session.Outcome{ExitCode: 1, SpendingCapAbort: true}
	}

	if err := e.CI(Target{Slug: "proj-1"})("", false); !errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("a capped fix session is the retry-after-reset class (BEH-571), got %v", err)
	}
}
