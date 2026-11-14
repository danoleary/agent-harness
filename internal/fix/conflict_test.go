package fix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/prompt"
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
	log := testLog(t)
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

// redBaseEnv is conflictEnv over a Consumer with one Gate, "check", whose
// resolution session resolves a real conflict, and whose shell runs answer
// shell: the re-gate over the resolved branch and any gate over the base.
func redBaseEnv(t *testing.T, shell func(hostio.ShellRun) session.Outcome) (Env, *hostio.Fake) {
	t.Helper()
	e, repo := conflictEnv(t, func(repo *gittest.Repo, h *hostio.Fake) { repo.Resolve(h.Worktree()) })
	e.Cfg.ProjectPath = t.TempDir()
	e.Cfg.Gates = []config.Gate{{Name: "check", Command: "make check"}}
	e.Who = "pr-fix"
	repo.Conflict("proj-1")
	if err := e.Host.FetchMain(); err != nil {
		t.Fatal(err)
	}
	h := e.Host.(*hostio.Fake)
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome { return shell(run) }
	return e, h
}

// mainDoesNotBuild is #250's shell: origin/main does not compile by itself (two
// PRs merged without seeing each other's change, so an interface gained a method
// one implementation lacks), so `make check` exits 2 over the resolved branch and
// over the base alike.
func mainDoesNotBuild(hostio.ShellRun) session.Outcome {
	return session.Outcome{ExitCode: 2, StderrTail: "internal/fake/fake.go:14:9: *Fake does not implement Port (missing method Close)"}
}

// #250, replayed: the session rebased the branch onto origin/main and resolved it
// cleanly, but origin/main does not build by itself, so the full re-gate went
// red on the base's compile error. Before, the run blamed the branch. Now the
// harness gates origin/main alone, finds it red on the same gate, and reports a
// red base: one confirmed, recorded verdict for that SHA, and nothing pushed.
func TestARedReGateOverARedBaseIsTheBasesNotTheBranchs(t *testing.T) {
	e, h := redBaseEnv(t, mainDoesNotBuild)
	wt := h.Worktree()
	base, err := wt.RefSHA("origin/main")
	if err != nil {
		t.Fatal(err)
	}

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

	if res.OK {
		t.Fatalf("a red re-gate must not authorise a push, got %+v", res)
	}
	if res.BaseRed == nil {
		t.Fatalf("a re-gate red on a gate origin/main fails too must report a red base, got %+v", res)
	}
	if res.BaseRed.SHA != base || res.BaseRed.Verdict.Gate != "check" || !res.BaseRed.Recorded {
		t.Errorf("BaseRed = %+v, want origin/main %s red on %q, recorded by this run", res.BaseRed, base, "check")
	}
	// The base is gated alone, in a worktree of its own, and its red is confirmed
	// by a re-run before it counts (#189).
	var onBase []string
	for _, s := range h.Shells {
		if strings.HasPrefix(s.Label, "basegate") {
			onBase = append(onBase, s.Label)
			if s.WorktreePath == wt.Path() {
				t.Errorf("gate %s ran over the resolved branch, want origin/main alone", s.Label)
			}
		}
	}
	if want := []string{"basegate-check", "basegate-confirm-check"}; strings.Join(onBase, ",") != strings.Join(want, ",") {
		t.Errorf("base gate runs = %v, want %v", onBase, want)
	}
	if v, ok := basegate.For(e.Cfg).Read(base); !ok || v.Gate != "check" {
		t.Errorf("recorded verdict for %s = %+v, %v; want red on %q, shared with the ticket loop", base, v, ok, "check")
	}
	got := runLog(t, e)
	if !strings.Contains(got, "base is red, not the branch") {
		t.Errorf("run.jsonl must say the base is red, not the branch; got %s", got)
	}
	if strings.Contains(got, "post-resolution gate re-run failed") {
		t.Errorf("run.jsonl must not blame the branch; got %s", got)
	}
}

// A base the verdict record already holds red on the same gate is not gated
// again: the record answers for that SHA.
func TestARedReGateReadsARecordedRedBase(t *testing.T) {
	e, h := redBaseEnv(t, mainDoesNotBuild)
	base, _ := h.Worktree().RefSHA("origin/main")
	if _, err := basegate.For(e.Cfg).Write(base, basegate.Verdict{Gate: "check"}); err != nil {
		t.Fatal(err)
	}

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

	if res.BaseRed == nil || res.BaseRed.Recorded {
		t.Fatalf("want a red base read from the record, got %+v", res)
	}
	for _, s := range h.Shells {
		if strings.HasPrefix(s.Label, "basegate") {
			t.Errorf("gated the recorded base again (%s)", s.Label)
		}
	}
}

// The branch is still to blame when the base passes the gate the re-gate failed,
// or fails a different one.
func TestARedReGateOverAGreenBaseIsTheBranchs(t *testing.T) {
	for name, baseGate := range map[string]string{"green base": "", "base red elsewhere": "lint"} {
		t.Run(name, func(t *testing.T) {
			e, h := redBaseEnv(t, mainDoesNotBuild)
			base, _ := h.Worktree().RefSHA("origin/main")
			if _, err := basegate.For(e.Cfg).Write(base, basegate.Verdict{Gate: baseGate}); err != nil {
				t.Fatal(err)
			}

			res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

			if res.OK || res.BaseRed != nil || !res.Resolved {
				t.Fatalf("want a resolved conflict whose own re-gate is red, got %+v", res)
			}
			if got := runLog(t, e); !strings.Contains(got, "post-resolution gate re-run failed") {
				t.Errorf("run.jsonl must blame the branch; got %s", got)
			}
		})
	}
}

// A base gated alone that passes is recorded green, and the branch keeps the
// blame for its red re-gate.
func TestARedReGateGatesTheBaseAndFindsItGreen(t *testing.T) {
	e, h := redBaseEnv(t, func(run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, "basegate") {
			return session.Outcome{}
		}
		return mainDoesNotBuild(run)
	})
	base, _ := h.Worktree().RefSHA("origin/main")

	res := e.ResolveConflict(Conflict{Regate: func() GateResult { return e.Gates("gate") }})

	if res.BaseRed != nil || res.OK {
		t.Fatalf("a green base leaves the red re-gate the branch's, got %+v", res)
	}
	if v, ok := basegate.For(e.Cfg).Read(base); !ok || !v.Green() {
		t.Errorf("recorded verdict = %+v, %v; want green", v, ok)
	}
}

// overlapsFrom is a resolution session that resolves the conflict and writes
// report, verbatim, as its overlap file; "" writes none.
func overlapsFrom(t *testing.T, report string) (Env, *hostio.Fake) {
	t.Helper()
	e, repo := conflictEnv(t, func(repo *gittest.Repo, h *hostio.Fake) {
		repo.Resolve(h.Worktree())
		run := h.Agents[len(h.Agents)-1]
		if report == "" {
			return
		}
		if run.FindingsDir == "" {
			t.Fatal("the replay session was given no /findings dropbox to report an overlap in")
		}
		if err := os.WriteFile(filepath.Join(run.FindingsDir, prompt.OverlapsFile), []byte(report), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	repo.Conflict("proj-1")
	if err := e.Host.FetchMain(); err != nil {
		t.Fatal(err)
	}
	return e, e.Host.(*hostio.Fake)
}

// #255, replayed: while the branch was in flight, main merged a hand-off-to-a-
// human capability (a new port method and a Disposition) in the area where the
// branch had built its own hand-off path. The session resolved the conflict but
// left the branch's parallel path in place, and reported it. The host reads that
// report back, so the PR can say so, and still clears the push.
func TestAResolutionCarriesTheOverlapItsSessionReported(t *testing.T) {
	e, _ := overlapsFrom(t, `[{"file": "internal/hostio/hostio.go", "main_added": "Host.HandToHuman and the NeedsHuman Disposition", "branch_duplicates": "Host.Escalate and its blocked-label breadcrumb"}]`)

	res := e.ResolveConflict(Conflict{Strategy: Rebase, Regate: func() GateResult { return GateResult{} }})

	if !res.OK {
		t.Fatalf("a reported overlap does not hold the push, got %+v", res)
	}
	want := []Overlap{{File: "internal/hostio/hostio.go", MainAdded: "Host.HandToHuman and the NeedsHuman Disposition", BranchDuplicates: "Host.Escalate and its blocked-label breadcrumb"}}
	if len(res.Overlaps) != 1 || res.Overlaps[0] != want[0] {
		t.Errorf("Overlaps = %+v, want %+v", res.Overlaps, want)
	}
	if got := runLog(t, e); !strings.Contains(got, "internal/hostio/hostio.go") || !strings.Contains(got, "#255") {
		t.Errorf("run.jsonl must name the overlapping file; got %s", got)
	}
}

// No file, an empty one or an empty list means no overlap; a file the host
// cannot parse is narrated and carries none. A report a prior resolution left in
// the dropbox is cleared before the session, so it is never carried twice.
func TestAResolutionWithNoUsableOverlapReportCarriesNone(t *testing.T) {
	for name, report := range map[string]string{"no file": "", "empty file": " \n", "empty list": "[]", "malformed": "{not json"} {
		t.Run(name, func(t *testing.T) {
			e, _ := overlapsFrom(t, report)
			stale := filepath.Join(e.Log.FindingsDir("rebasefix"), prompt.OverlapsFile)
			if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
				t.Fatal(err)
			}
			if report == "" {
				if err := os.WriteFile(stale, []byte(`[{"file": "stale.go", "main_added": "x", "branch_duplicates": "y"}]`), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			res := e.ResolveConflict(Conflict{Strategy: Rebase, Regate: func() GateResult { return GateResult{} }})

			if !res.OK || len(res.Overlaps) != 0 {
				t.Fatalf("want a clear push with no overlap, got %+v", res)
			}
			if got := runLog(t, e); (name == "malformed") != strings.Contains(got, "could not read the conflict resolution's overlap report") {
				t.Errorf("only a malformed report is narrated as unreadable; got %s", got)
			}
		})
	}
}

// The merge variant is unchanged (#255): it gets no dropbox to write a report in.
func TestAMergeResolutionMountsNoOverlapDropbox(t *testing.T) {
	e, h := overlapsFrom(t, "")

	e.ResolveConflict(Conflict{Strategy: Merge, Regate: func() GateResult { return GateResult{} }})

	if got := h.Agents[0].FindingsDir; got != "" {
		t.Errorf("merge resolution FindingsDir = %q, want none", got)
	}
}
