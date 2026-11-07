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

// cleared is the push gate a review whose Gates were green and which declared no
// blocked Disposition leaves over the branch's current tip: the one record that
// lets a recovery land it.
func cleared(t *testing.T, s scene) *ship.PushGate {
	t.Helper()
	head, err := s.wt.HeadSHA()
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	return &ship.PushGate{Head: head, Disposition: "Disposition: clear — no findings", GateRan: true}
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

// A landing with no ticket opens the PR its caller describes (ADR-0040): the title
// as given, and a body built from the commit subjects the replay left on the
// branch and the conflict hook's note. Land is the ticket's Opening over LandAs.
func TestLandAsOpensThePRItsCallerDescribes(t *testing.T) {
	s := newScene(t)
	s.repo.Conflict("proj-1")
	var gotSubjects []string
	var gotNote string
	opening := ship.Opening{
		Title: "Fix 2 security alerts in lodash",
		Body: func(subjects []string, note string) string {
			gotSubjects, gotNote = subjects, note
			return "bumps lodash"
		},
	}

	landing := ship.LandAs(s.h, &sink{}, opening, func([]string) ship.Resolution {
		s.repo.Resolve(s.wt)
		return ship.Resolution{OK: true, Note: "resolved after review"}
	})

	if landing.Verdict != ship.Landed {
		t.Fatalf("Verdict = %v, want Landed", landing.Verdict)
	}
	if c := calls(s.h); !strings.HasSuffix(c, "create-pr Fix 2 security alerts in lodash") {
		t.Errorf("calls = %q, want the PR opened under the Opening's title", c)
	}
	if len(s.h.PRBodies) != 1 || s.h.PRBodies[0] != "bumps lodash" {
		t.Errorf("PR bodies = %q, want the Opening's body", s.h.PRBodies)
	}
	if len(gotSubjects) == 0 || gotSubjects[0] != gittest.HandoffSubject {
		t.Errorf("subjects = %q, want the branch's commit subjects led by %q", gotSubjects, gittest.HandoffSubject)
	}
	if gotNote != "resolved after review" {
		t.Errorf("note = %q, want the conflict hook's note", gotNote)
	}
}

// A landing that stops before the push never builds the body: there is no PR.
func TestLandAsBuildsNoBodyWhenNothingIsPushed(t *testing.T) {
	s := newScene(t)
	s.repo.Supersede("proj-1")
	built := false

	landing := ship.LandAs(s.h, &sink{}, ship.Opening{Title: "t", Body: func([]string, string) string { built = true; return "" }}, nil)

	if landing.Verdict != ship.Collapsed {
		t.Fatalf("Verdict = %v, want Collapsed", landing.Verdict)
	}
	if built {
		t.Error("the body must not be built for a branch that was never pushed")
	}
}

// --- Replay -----------------------------------------------------------------

// inTheWay gives the branch a second commit and makes the next times replays stop
// on it with no pick in progress and nothing unmerged.
func inTheWay(s scene, times int) {
	s.repo.Commit(s.wt, "second.txt", "more\n", "the second commit")
	s.repo.InTheWayOfReplay(s.wt, "second.txt", times)
}

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
		// #164: git failing with no content conflict is retried once, not reported as
		// a conflict.
		{"git fails once, retried", func(s scene) { inTheWay(s, 1) }, ship.ReplayClean, false},
		{"git keeps failing", func(s scene) { inTheWay(s, 2) }, ship.ReplayFailed, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newScene(t)
			tc.scene(s)
			if err := s.h.FetchMain(); err != nil {
				t.Fatalf("FetchMain: %v", err)
			}
			before, _ := s.wt.HeadSHA()

			if got := ship.Replay(s.h, &sink{}); got != tc.want {
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

			if got := ship.Recover(h, log, "PROJ-1", nil); got.Attempted || got.Shipped {
				t.Fatalf("Recover() = %+v, want the zero Recovery (the daemon releases as before)", got)
			}
			if c := calls(h); strings.Contains(c, "push") || strings.Contains(c, "create-pr") {
				t.Errorf("calls = %v, want no push/PR for a branch with nothing to finish", h.Calls)
			}
		})
	}
}

// A committed tip no host gate covers is not the gate-green state the recovery
// exists to finish: the review never ran over it (a checkpoint commit after a
// killed review, a run from before the push gate was recorded), so a later run must
// review it. Recovery declines and the daemon releases as before, touching no
// remote.
func TestRecoverDeclinesATipNoGateCovers(t *testing.T) {
	cases := []struct {
		name string
		gate func(t *testing.T, s scene) *ship.PushGate
	}{
		{"no push gate recorded", func(*testing.T, scene) *ship.PushGate { return nil }},
		{"the gate covered an earlier tip", func(t *testing.T, s scene) *ship.PushGate {
			g := cleared(t, s)
			g.Head = s.repo.Git(s.repo.Dir, "rev-parse", gitpkg.BaseRef)
			return g
		}},
		{"a verdict with no gate run", func(t *testing.T, s scene) *ship.PushGate {
			g := cleared(t, s)
			g.GateRan = false
			return g
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, log := recoverable(t), &sink{}

			got := ship.Recover(s.h, log, "PROJ-1", tc.gate(t, s))

			if got != (ship.Recovery{}) {
				t.Fatalf("Recover() = %+v, want the zero Recovery (the daemon releases as before)", got)
			}
			if c := calls(s.h); strings.Contains(c, "push") || strings.Contains(c, "create-pr") {
				t.Errorf("calls = %v, want no push/PR for a tip no gate covers", s.h.Calls)
			}
			if !log.saw("no host gate covers") {
				t.Errorf("events = %v, want the uncovered tip narrated", log.events)
			}
		})
	}
}

// The two herd incidents behind #230, replayed: a review that refused to ship left a
// clean, committed worktree with no PR, and the recovery pushed it anyway a minute
// later. A push gate that was blocked or red over the very tip the recovery would
// land is a refusal, not a stranded fix: nothing is pushed, the worktree is kept for
// the human, and the reason comes back for the daemon to hand the ticket over with.
func TestRecoverRefusesABranchReviewDidNotClear(t *testing.T) {
	cases := []struct {
		name   string
		gate   func(g *ship.PushGate)
		reason []string
	}{
		// BEH-1305: review wrote Disposition: blocked, and the unit gate was red.
		{"blocked verdict (BEH-1305)", func(g *ship.PushGate) {
			g.Disposition = "Disposition: blocked — the sandbox cannot regenerate database.types.ts, so a human must"
			g.Blocked = true
			g.GateExit = 1
			g.FailedGate = "unit"
		}, []string{"blocked", "database.types.ts", `"unit"`}},
		// BEH-1198: review stopped on a red storybook gate and emitted no verdict.
		{"red gate (BEH-1198)", func(g *ship.PushGate) {
			g.Disposition = ""
			g.GateExit = 2
			g.FailedGate = "storybook"
		}, []string{`"storybook"`, "red"}},
		{"red gate that could not name itself", func(g *ship.PushGate) {
			g.GateExit = 127
			g.FailedGate = ""
		}, []string{"exit 127"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, log := recoverable(t), &sink{}
			g := cleared(t, s)
			tc.gate(g)

			got := ship.Recover(s.h, log, "PROJ-1", g)

			if !got.Attempted || got.Shipped || got.Refused == "" {
				t.Fatalf("Recover() = %+v, want an attempted recovery that refused", got)
			}
			for _, want := range tc.reason {
				if !strings.Contains(got.Refused, want) {
					t.Errorf("Refused = %q, want it to name %q", got.Refused, want)
				}
			}
			if c := calls(s.h); strings.Contains(c, "push") || strings.Contains(c, "create-pr") {
				t.Errorf("calls = %v, want nothing pushed and no PR for a branch review did not clear", s.h.Calls)
			}
			if s.repo.OnOrigin("feat/proj-1") != "" {
				t.Error("a refused branch must not reach origin")
			}
			if !s.wt.Exists() {
				t.Error("a refused recovery must keep the worktree for the human")
			}
			if !log.saw("refusing to land") {
				t.Errorf("events = %v, want the refusal narrated", log.events)
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

	got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s))

	if !got.Attempted || !got.Shipped {
		t.Fatalf("Recover() = %+v, want an attempted recovery that shipped", got)
	}
	want := "push-force create-pr PROJ-1: a ticket"
	if c := calls(s.h); !strings.Contains(c, want) {
		t.Errorf("calls = %v,\nwant the sequence %q", s.h.Calls, want)
	}
	pushed := s.repo.OnOrigin("feat/proj-1")
	if pushed == "" || s.repo.Git(s.repo.Dir, "merge-base", gitpkg.BaseRef, pushed) != s.repo.Git(s.repo.Dir, "rev-parse", gitpkg.BaseRef) {
		t.Error("the fix must be replayed onto the latest main and pushed")
	}
	if !log.saw("completing it host-side") {
		t.Errorf("events = %v, want the recovery announced", log.events)
	}
	// The slice's retrospective kept the worktree only so this recovery could finish
	// it; with the PR open the work has escaped it, so recovery reaps it (#184).
	if s.wt.Exists() {
		t.Error("a shipped recovery must reap the worktree, or the ticket loop holds its branch forever")
	}
}

// A content conflict can't be auto-resolved here (that is the review stage's
// sandboxed job): the worktree is left on its branch and clean for the next run,
// and the recovery reports attempted-but-not-shipped.
func TestRecoverAbortsOnARebaseConflict(t *testing.T) {
	s, log := recoverable(t), &sink{}
	s.repo.Conflict("proj-1")
	before, _ := s.wt.HeadSHA()

	got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s))

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

	got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s))

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

	got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s))

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

	got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s))

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

			got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s))

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

	if got := ship.Recover(s.h, log, "PROJ-1", cleared(t, s)); !got.Shipped {
		t.Fatalf("Recover() = %+v, want the fix shipped despite the fetch warning", got)
	}
	if !log.saw("could not fetch origin/main") {
		t.Errorf("events = %v, want the fetch failure warned", log.events)
	}
}
