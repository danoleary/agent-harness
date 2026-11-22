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

	if err := e.CI(Target{Slug: "proj-1"})(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true}); err != nil {
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

	err := e.CI(Target{Slug: "proj-1"})(ci.FixInput{Logs: "FAIL TestThing", LogAvailable: true})

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

	if err := e.CI(Target{Slug: "proj-1"})(ci.FixInput{Logs: "", LogAvailable: false}); !errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("a capped fix session is the retry-after-reset class (BEH-571), got %v", err)
	}
}

// #397: the watch's red-on-rerun flag reaches the CIFix session's prompt, so a
// check that failed on two CI attempts of one commit is not read as a one-off.
func TestACIFixOverARedRerunTellsTheSessionTheCheckFailedTwice(t *testing.T) {
	for _, redOnRerun := range []bool{true, false} {
		e, _ := conflictEnv(t, func(*gittest.Repo, *hostio.Fake) {})
		h := e.Host.(*hostio.Fake)

		if err := e.CI(Target{Slug: "proj-1"})(ci.FixInput{Logs: "FAIL TestRace", LogAvailable: true, RedOnRerun: redOnRerun}); err != nil {
			t.Fatalf("redOnRerun=%v: %v", redOnRerun, err)
		}
		if got := strings.Contains(h.Agents[0].Prompt, "already failed on a CI re-run of the same commit"); got != redOnRerun {
			t.Errorf("redOnRerun=%v: prompt names the red re-run = %v", redOnRerun, got)
		}
	}
}
