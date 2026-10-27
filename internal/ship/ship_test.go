package ship_test

import (
	"errors"
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/ship"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// sink records the narration so a test can assert what an operator would read in
// loop.log — the only window onto a recovery that runs unattended.
type sink struct{ events []string }

func (s *sink) Event(msg string) { s.events = append(s.events, msg) }

func (s *sink) saw(substr string) bool {
	for _, e := range s.events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// scene is a ticket's git state and the host over it: PROJ-1's worktree carrying
// one committed handoff against origin/main, in a real repo, with NewFake's healthy
// remote. A test changes the state through repo — a sibling's merge on main, a
// disjoint root — and git, not a flag, decides how the landing goes.
type scene struct {
	h    *hostio.Fake
	repo *gittest.Repo
	wt   gitpkg.Worktree
}

func newScene(t *testing.T) scene {
	t.Helper()
	repo := gittest.New(t)
	wt := repo.Handoff("proj-1")
	return scene{h: hostio.NewFake(repo.Checkout(), "proj-1"), repo: repo, wt: wt}
}

// recoverable is the BEH-713 state: a clean worktree carrying committed work
// against origin/main, with no open PR. NewFake answers gh with a PR already open,
// so the one field that makes a branch *strandable* is the one a recovery test has
// to flip.
func recoverable(t *testing.T) scene {
	t.Helper()
	s := newScene(t)
	s.h.OpenPRThere = false
	return s
}

func calls(h *hostio.Fake) string { return strings.Join(h.Calls, " ") }

// --- Land -------------------------------------------------------------------

// The tracer bullet: landing a branch is refetch → replay → force-with-lease push
// → `gh pr create`, in that order, and it hands back the PR URL the caller narrates.
func TestLandRebasesPushesAndOpensThePR(t *testing.T) {
	s, log := newScene(t), &sink{}
	s.repo.Merge("other.txt", "unrelated\n", "a sibling merged")

	got := ship.Land(s.h, log, ticket.Ticket{Identifier: "PROJ-1", Title: "a ticket"}, nil)

	if got.Verdict != ship.Landed || got.Err != nil {
		t.Fatalf("Land() = %+v, want Landed", got)
	}
	if got.URL != s.h.PRURL {
		t.Errorf("URL = %q, want the created PR's URL %q", got.URL, s.h.PRURL)
	}
	want := "fetch-main push-force create-pr PROJ-1: a ticket"
	if c := calls(s.h); c != want {
		t.Errorf("calls = %q,\nwant %q", c, want)
	}
	if !s.wt.IsRebasedOnto(gitpkg.BaseRef) {
		t.Error("the branch must be replayed onto the main it just fetched")
	}
	if tip, _ := s.wt.HeadSHA(); s.repo.OnOrigin("feat/proj-1") != tip {
		t.Error("the replayed tip must be what reached origin")
	}
	if !log.saw("rebased feat/proj-1 onto origin/main") || !log.saw("pushed feat/proj-1 to origin") {
		t.Errorf("events = %v, want the rebase and the push narrated", log.events)
	}
}

// The verdict table: every way a landing can stop, what it leaves behind, and
// whether anything reached origin. Every caller maps exactly these verdicts, so
// a guard fixed here is fixed for the review, the recovery and the CI watch alike.
func TestLandVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		scene    func(scene)
		resolve  func(scene) ship.Resolve
		want     ship.Verdict
		pushed   bool // did the branch reach origin?
		prCalled bool // did a create-pr reach the host?
	}{
		{"clean", func(scene) {}, nil, ship.Landed, true, true},
		{"fetch failure is a warning", func(s scene) { s.h.FetchErr = errors.New("net down") }, nil, ship.Landed, true, true},
		{"conflict, no hook", func(s scene) { s.repo.Conflict("proj-1") }, nil, ship.Conflict, false, false},
		{"conflict, hook gives up", func(s scene) { s.repo.Conflict("proj-1") }, func(scene) ship.Resolve { return func([]string) ship.Resolution { return ship.Resolution{} } }, ship.Conflict, false, false},
		{"conflict, hook resolves", func(s scene) { s.repo.Conflict("proj-1") }, func(s scene) ship.Resolve {
			return func([]string) ship.Resolution { s.repo.Resolve(s.wt); return ship.Resolution{OK: true} }
		}, ship.Landed, true, true},
		{"disjoint history", func(s scene) { s.repo.Disjoint(s.wt) }, nil, ship.Disjoint, false, false},
		{"replay collapses the branch", func(s scene) { s.repo.Supersede("proj-1") }, nil, ship.Collapsed, false, false},
		{"push rejected", func(s scene) { s.h.ForcePushErr = errors.New("remote rejected") }, nil, ship.PushFailed, false, false},
		{"gh pr create fails", func(s scene) { s.h.CreatePRErr = errors.New("rate limited") }, nil, ship.PRFailed, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(t)
			tc.scene(s)
			var resolve ship.Resolve
			if tc.resolve != nil {
				resolve = tc.resolve(s)
			}

			got := ship.Land(s.h, &sink{}, ticket.Ticket{Identifier: "PROJ-1"}, resolve)

			if got.Verdict != tc.want {
				t.Fatalf("Verdict = %v, want %v (landing %+v)", got.Verdict, tc.want, got)
			}
			if (got.Err != nil) != (tc.want == ship.PushFailed || tc.want == ship.PRFailed) {
				t.Errorf("Err = %v, want an error exactly on PushFailed/PRFailed", got.Err)
			}
			if pushed := s.repo.OnOrigin("feat/proj-1") != ""; pushed != tc.pushed {
				t.Errorf("branch on origin = %v, want %v (calls %v)", pushed, tc.pushed, s.h.Calls)
			}
			if strings.Contains(calls(s.h), "create-pr") != tc.prCalled {
				t.Errorf("calls = %v, PR create attempted = %v, want %v", s.h.Calls, !tc.prCalled, tc.prCalled)
			}
		})
	}
}

// A disjoint history is not a content conflict (BEH-597): no resolution session
// can change a branch's root commit, so the hook must never be offered one.
func TestLandNeverHandsADisjointBranchToTheConflictHook(t *testing.T) {
	s := newScene(t)
	s.repo.Disjoint(s.wt)
	called := false

	ship.Land(s.h, &sink{}, ticket.Ticket{}, func([]string) ship.Resolution { called = true; return ship.Resolution{OK: true} })

	if called {
		t.Error("the conflict hook ran for a disjoint branch; it must only see content conflicts")
	}
}

// The conflict hook is told which files git reported as conflicted, so the caller
// can tell the resolution's edits to them from its edits to anything else (#114),
// and the note it returns reaches the PR body: a human reading the PR learns what
// was written after review.
func TestLandHandsTheHookTheConflictedFilesAndCarriesItsNoteToThePR(t *testing.T) {
	s := newScene(t)
	s.repo.Conflict("proj-1")
	var got []string

	landing := ship.Land(s.h, &sink{}, ticket.Ticket{Identifier: "PROJ-1"}, func(conflicted []string) ship.Resolution {
		got = conflicted
		s.repo.Resolve(s.wt)
		return ship.Resolution{OK: true, Note: "written after review"}
	})

	if landing.Verdict != ship.Landed {
		t.Fatalf("Verdict = %v, want Landed", landing.Verdict)
	}
	if len(got) != 1 || got[0] != "proj-1.txt" {
		t.Errorf("conflicted = %v, want [proj-1.txt]", got)
	}
	if len(s.h.PRBodies) != 1 || !strings.Contains(s.h.PRBodies[0], "written after review") {
		t.Errorf("PR bodies = %q, want the hook's note in the body", s.h.PRBodies)
	}
}

// --- Replay -----------------------------------------------------------------

// The rebase half the CI watch reuses: whatever stops the replay, the worktree is
// left clean on its branch's original tip, so nothing is left mid-replay.
func TestReplay(t *testing.T) {
	cases := []struct {
		name     string
		scene    func(scene)
		want     ship.Replayed
		restored bool
	}{
		{"clean", func(scene) {}, ship.ReplayClean, false},
		{"conflict", func(s scene) { s.repo.Conflict("proj-1") }, ship.ReplayConflict, true},
		{"disjoint", func(s scene) { s.repo.Disjoint(s.wt) }, ship.ReplayDisjoint, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(t)
			tc.scene(s)
			if err := s.h.FetchMain(); err != nil {
				t.Fatalf("FetchMain: %v", err)
			}
			before, _ := s.wt.HeadSHA()

			if got := ship.Replay(s.h); got != tc.want {
				t.Fatalf("Replay() = %v, want %v", got, tc.want)
			}
			if !tc.restored {
				return
			}
			if after, _ := s.wt.HeadSHA(); after != before || !s.wt.Clean() {
				t.Errorf("a stopped replay must leave the branch clean on its tip %s, got %s (clean %v)", before, after, s.wt.Clean())
			}
		})
	}
}

// --- Recover: nothing to finish ---------------------------------------------

// Each of these is a genuine no-PR run — an OOM, a crash, an empty diff, or a
// branch that already shipped. Recovery must decline (Attempted=false) so the
// daemon releases the ticket to Todo exactly as it did before BEH-713, and must
// touch no remote on the way out.
func TestRecoverDeclinesWhenThereIsNothingToFinish(t *testing.T) {
	cases := []struct {
		name  string
		scene func(t *testing.T) *hostio.Fake
	}{
		{"no worktree", func(t *testing.T) *hostio.Fake {
			return hostio.NewFake(gittest.New(t).Checkout(), "proj-1")
		}},
		{"uncommitted changes", func(t *testing.T) *hostio.Fake {
			s := recoverable(t)
			s.repo.Dirty(s.wt)
			return s.h
		}},
		{"nothing ahead of main", func(t *testing.T) *hostio.Fake {
			repo := gittest.New(t)
			if err := repo.Worktree("proj-1").Create(); err != nil {
				t.Fatalf("Create: %v", err)
			}
			h := hostio.NewFake(repo.Checkout(), "proj-1")
			h.OpenPRThere = false
			return h
		}},
		{"already has an open PR", func(t *testing.T) *hostio.Fake { return newScene(t).h }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, log := tc.scene(t), &sink{}

			if got := ship.Recover(h, log, "PROJ-1"); got.Attempted || got.Shipped {
				t.Fatalf("Recover() = %+v, want the zero Recovery (the daemon releases as before)", got)
			}
			if c := calls(h); strings.Contains(c, "push") || strings.Contains(c, "create-pr") {
				t.Errorf("calls = %v, want no push/PR for a branch with nothing to finish", h.Calls)
			}
		})
	}
}

// --- Recover: the BEH-713 completion ----------------------------------------

// The tracer bullet for the whole recovery: a committed, unpushed, gate-green
// branch with no PR is rebased onto the latest main and shipped, instead of being
// released to Todo to loop into the same conclusion forever (the BEH-649 bounce).
func TestRecoverCompletesACommittedUnpushedFix(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.repo.Merge("other.txt", "unrelated\n", "a sibling merged")

	got := ship.Recover(s.h, log, "PROJ-1")

	if !got.Attempted || !got.Shipped {
		t.Fatalf("Recover() = %+v, want an attempted recovery that shipped", got)
	}
	want := "push-force create-pr PROJ-1: a ticket"
	if c := calls(s.h); !strings.Contains(c, want) {
		t.Errorf("calls = %v,\nwant the sequence %q", s.h.Calls, want)
	}
	if !s.wt.IsRebasedOnto(gitpkg.BaseRef) || s.repo.OnOrigin("feat/proj-1") == "" {
		t.Error("the fix must be replayed onto the latest main and pushed")
	}
	if !log.saw("completing it host-side") {
		t.Errorf("events = %v, want the recovery announced", log.events)
	}
}

// A content conflict can't be auto-resolved here (that is the review stage's
// sandboxed job): the worktree is left on its branch and clean for the next run,
// and the recovery reports attempted-but-not-shipped.
func TestRecoverAbortsOnARebaseConflict(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.repo.Conflict("proj-1")
	before, _ := s.wt.HeadSHA()

	got := ship.Recover(s.h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if after, _ := s.wt.HeadSHA(); after != before || !s.wt.Clean() {
		t.Errorf("the conflicted replay must leave the branch clean on %s, got %s", before, after)
	}
	if s.repo.OnOrigin("feat/proj-1") != "" {
		t.Error("nothing may be pushed after a conflict")
	}
	if !log.saw("rebase onto main conflicted") {
		t.Errorf("events = %v, want the conflict narrated", log.events)
	}
}

// The disjoint-history guard used to live only in the review stage (BEH-597); a
// recovery now gets it too, and says so rather than calling it a conflict.
func TestRecoverNamesADisjointBranch(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.repo.Disjoint(s.wt)

	got := ship.Recover(s.h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if s.repo.OnOrigin("feat/proj-1") != "" {
		t.Error("nothing may be pushed for a disjoint branch")
	}
	if !log.saw("disjoint history") {
		t.Errorf("events = %v, want the disjoint history named", log.events)
	}
}

// A sibling PR can land the same fix while the branch sat unpushed, so the rebase
// empties it. `gh pr create` on that branch hard-fails ("No commits between main
// and feat/…"), so the recovery must stop before the push. Main moved on past the
// sibling too, or the pre-landing empty-diff check would already have declined.
func TestRecoverStopsWhenTheRebaseEmptiesTheBranch(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.repo.Supersede("proj-1")
	s.repo.Merge("other.txt", "unrelated\n", "main moved on")

	got := ship.Recover(s.h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if strings.Contains(calls(s.h), "push-force") {
		t.Errorf("calls = %v, want nothing pushed for a branch that became empty", s.h.Calls)
	}
	if !log.saw("became empty after rebase") {
		t.Errorf("events = %v, want the empty branch narrated", log.events)
	}
}

// The ticket is fetched for the PR body BEFORE the push, so a tracker hiccup
// leaves the branch exactly as it was rather than pushed with no PR — which is
// the very state this recovery exists to clean up.
func TestRecoverDoesNotPushWhenTheTicketCannotBeFetched(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.h.Trk.FetchErr = errors.New("linear boom")

	got := ship.Recover(s.h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if strings.Contains(calls(s.h), "push-force") {
		t.Errorf("calls = %v, want nothing pushed when the PR body can't be built", s.h.Calls)
	}
	if !log.saw("could not fetch ticket") {
		t.Errorf("events = %v, want the tracker failure narrated", log.events)
	}
}

// A push or PR failure mid-recovery is never fatal: it falls through to the
// daemon's normal release (attempted, not shipped) so a later run re-grabs the
// ticket and retries — an attempt is never worse than no attempt.
func TestRecoverFallsThroughWhenShippingFails(t *testing.T) {
	cases := []struct {
		name    string
		scene   func(*hostio.Fake)
		narrate string
	}{
		{"push rejected", func(h *hostio.Fake) { h.ForcePushErr = errors.New("non-fast-forward") }, "push failed"},
		{"gh pr create failed", func(h *hostio.Fake) { h.CreatePRErr = errors.New("rate limited") }, "gh pr create failed"},
		{"no tracker", func(h *hostio.Fake) { h.TrackerErr = errors.New("no LINEAR_API_KEY") }, "no tracker for the PR body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, log := recoverable(t), &sink{}
			tc.scene(s.h)

			got := ship.Recover(s.h, log, "PROJ-1")

			if !got.Attempted || got.Shipped {
				t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
			}
			if !log.saw(tc.narrate) {
				t.Errorf("events = %v, want %q narrated", log.events, tc.narrate)
			}
		})
	}
}

// A fetch failure is a warning, not a stop: origin/main may be a few minutes
// stale, which only risks a noisier rebase — the committed fix still ships.
func TestRecoverShipsThroughAFailedFetch(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.h.FetchErr = errors.New("network down")

	if got := ship.Recover(s.h, log, "PROJ-1"); !got.Shipped {
		t.Fatalf("Recover() = %+v, want the fix shipped despite the fetch warning", got)
	}
	if !log.saw("could not fetch origin/main") {
		t.Errorf("events = %v, want the fetch failure warned", log.events)
	}
}
