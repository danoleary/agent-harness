package ship_test

import (
	"errors"
	"strings"
	"testing"

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

// recoverable returns a Fake scripted as the BEH-713 state: a clean worktree
// carrying committed work against origin/main, with no open PR. NewFake is a
// healthy host with a PR already open, so the one field that makes a branch
// *strandable* is the one a recovery test has to flip.
func recoverable() *hostio.Fake {
	h := hostio.NewFake()
	h.OpenPRThere = false
	return h
}

func calls(h *hostio.Fake) string { return strings.Join(h.Calls, " ") }

// --- Land -------------------------------------------------------------------

// The tracer bullet: landing a branch is refetch → replay → force-with-lease push
// → `gh pr create`, in that order, and it hands back the PR URL the caller narrates.
func TestLandRebasesPushesAndOpensThePR(t *testing.T) {
	h, log := hostio.NewFake(), &sink{}
	got := ship.Land(h, log, "proj-1", ticket.Ticket{Identifier: "PROJ-1", Title: "a ticket"}, nil)

	if got.Verdict != ship.Landed || got.Err != nil {
		t.Fatalf("Land() = %+v, want Landed", got)
	}
	if got.URL != h.PRURL {
		t.Errorf("URL = %q, want the created PR's URL %q", got.URL, h.PRURL)
	}
	want := "fetch-main rebase proj-1 push-force proj-1 create-pr PROJ-1: a ticket"
	if c := calls(h); c != want {
		t.Errorf("calls = %q,\nwant %q", c, want)
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
		scene    func(*hostio.Fake)
		resolve  ship.Resolve
		want     ship.Verdict
		pushed   bool // did a push-force reach the host?
		prCalled bool // did a create-pr reach the host?
	}{
		{"clean", func(*hostio.Fake) {}, nil, ship.Landed, true, true},
		{"fetch failure is a warning", func(h *hostio.Fake) { h.FetchErr = errors.New("net down") }, nil, ship.Landed, true, true},
		{"conflict, no hook", func(h *hostio.Fake) { h.RebaseVerdict = hostio.RebaseConflict }, nil, ship.Conflict, false, false},
		{"conflict, hook gives up", func(h *hostio.Fake) { h.RebaseVerdict = hostio.RebaseConflict }, func() bool { return false }, ship.Conflict, false, false},
		{"conflict, hook resolves", func(h *hostio.Fake) { h.RebaseVerdict = hostio.RebaseConflict }, func() bool { return true }, ship.Landed, true, true},
		{"disjoint history", func(h *hostio.Fake) { h.RebaseVerdict = hostio.RebaseConflict; h.Disjoint = true }, nil, ship.Disjoint, false, false},
		{"replay collapses the branch", func(h *hostio.Fake) { h.CollapseOnRebase = true }, nil, ship.Collapsed, false, false},
		{"push rejected", func(h *hostio.Fake) { h.ForcePushErr = errors.New("remote rejected") }, nil, ship.PushFailed, true, false},
		{"gh pr create fails", func(h *hostio.Fake) { h.CreatePRErr = errors.New("rate limited") }, nil, ship.PRFailed, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hostio.NewFake()
			tc.scene(h)

			got := ship.Land(h, &sink{}, "proj-1", ticket.Ticket{Identifier: "PROJ-1"}, tc.resolve)

			if got.Verdict != tc.want {
				t.Fatalf("Verdict = %v, want %v (landing %+v)", got.Verdict, tc.want, got)
			}
			if (got.Err != nil) != (tc.want == ship.PushFailed || tc.want == ship.PRFailed) {
				t.Errorf("Err = %v, want an error exactly on PushFailed/PRFailed", got.Err)
			}
			c := calls(h)
			if strings.Contains(c, "push-force") != tc.pushed {
				t.Errorf("calls = %v, push attempted = %v, want %v", h.Calls, !tc.pushed, tc.pushed)
			}
			if strings.Contains(c, "create-pr") != tc.prCalled {
				t.Errorf("calls = %v, PR create attempted = %v, want %v", h.Calls, !tc.prCalled, tc.prCalled)
			}
		})
	}
}

// A disjoint history is not a content conflict (BEH-597): no resolution session
// can change a branch's root commit, so the hook must never be offered one.
func TestLandNeverHandsADisjointBranchToTheConflictHook(t *testing.T) {
	h := hostio.NewFake()
	h.RebaseVerdict, h.Disjoint = hostio.RebaseConflict, true
	called := false

	ship.Land(h, &sink{}, "proj-1", ticket.Ticket{}, func() bool { called = true; return true })

	if called {
		t.Error("the conflict hook ran for a disjoint branch; it must only see content conflicts")
	}
}

// --- Replay -----------------------------------------------------------------

// The rebase half the CI watch reuses: whatever stops the replay, the worktree is
// restored to its branch so nothing is left mid-replay.
func TestReplay(t *testing.T) {
	cases := []struct {
		name     string
		scene    func(*hostio.Fake)
		want     ship.Replayed
		restored bool
	}{
		{"clean", func(*hostio.Fake) {}, ship.ReplayClean, false},
		{"conflict", func(h *hostio.Fake) { h.RebaseVerdict = hostio.RebaseConflict }, ship.ReplayConflict, true},
		{"disjoint", func(h *hostio.Fake) { h.RebaseVerdict = hostio.RebaseConflict; h.Disjoint = true }, ship.ReplayDisjoint, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hostio.NewFake()
			tc.scene(h)

			if got := ship.Replay(h, "proj-1"); got != tc.want {
				t.Fatalf("Replay() = %v, want %v", got, tc.want)
			}
			if strings.Contains(calls(h), "abort-rebase proj-1") != tc.restored {
				t.Errorf("calls = %v, want abort-rebase = %v", h.Calls, tc.restored)
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
		scene func(*hostio.Fake)
	}{
		{"no worktree", func(h *hostio.Fake) { h.Exists = false }},
		{"uncommitted changes", func(h *hostio.Fake) { h.Clean = false }},
		{"nothing ahead of main", func(h *hostio.Fake) { h.DiffEmpty = true }},
		{"already has an open PR", func(h *hostio.Fake) { h.OpenPRThere = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, log := recoverable(), &sink{}
			tc.scene(h)

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
	h, log := recoverable(), &sink{}

	got := ship.Recover(h, log, "PROJ-1")

	if !got.Attempted || !got.Shipped {
		t.Fatalf("Recover() = %+v, want an attempted recovery that shipped", got)
	}
	want := "fetch-main rebase proj-1 push-force proj-1 create-pr PROJ-1: a ticket"
	if c := calls(h); !strings.Contains(c, want) {
		t.Errorf("calls = %v,\nwant the sequence %q (refresh main, rebase onto it, then ship)", h.Calls, want)
	}
	if !log.saw("completing it host-side") {
		t.Errorf("events = %v, want the recovery announced", log.events)
	}
}

// A content conflict can't be auto-resolved here (that is the review stage's
// sandboxed job): abort the rebase so the worktree is left on its branch and
// clean for the next run, and report attempted-but-not-shipped.
func TestRecoverAbortsOnARebaseConflict(t *testing.T) {
	h, log := recoverable(), &sink{}
	h.RebaseVerdict = hostio.RebaseConflict

	got := ship.Recover(h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if !strings.Contains(calls(h), "abort-rebase proj-1") {
		t.Errorf("calls = %v, want the conflicted rebase aborted", h.Calls)
	}
	if strings.Contains(calls(h), "push-force") {
		t.Errorf("calls = %v, want nothing pushed after a conflict", h.Calls)
	}
	if !log.saw("rebase onto main conflicted") {
		t.Errorf("events = %v, want the conflict narrated", log.events)
	}
}

// The disjoint-history guard used to live only in the review stage (BEH-597); a
// recovery now gets it too, and says so rather than calling it a conflict.
func TestRecoverNamesADisjointBranch(t *testing.T) {
	h, log := recoverable(), &sink{}
	h.RebaseVerdict, h.Disjoint = hostio.RebaseConflict, true

	got := ship.Recover(h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if strings.Contains(calls(h), "push-force") {
		t.Errorf("calls = %v, want nothing pushed for a disjoint branch", h.Calls)
	}
	if !log.saw("disjoint history") {
		t.Errorf("events = %v, want the disjoint history named", log.events)
	}
}

// A sibling PR can land the same fix while the branch sat unpushed, so the rebase
// empties it. `gh pr create` on that branch hard-fails ("No commits between main
// and feat/…"), so the recovery must stop before the push.
func TestRecoverStopsWhenTheRebaseEmptiesTheBranch(t *testing.T) {
	h, log := recoverable(), &sink{}
	h.CollapseOnRebase = true

	got := ship.Recover(h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if strings.Contains(calls(h), "push-force") {
		t.Errorf("calls = %v, want nothing pushed for a branch that became empty", h.Calls)
	}
	if !log.saw("became empty after rebase") {
		t.Errorf("events = %v, want the empty branch narrated", log.events)
	}
}

// The ticket is fetched for the PR body BEFORE the push, so a tracker hiccup
// leaves the branch exactly as it was rather than pushed with no PR — which is
// the very state this recovery exists to clean up.
func TestRecoverDoesNotPushWhenTheTicketCannotBeFetched(t *testing.T) {
	h, log := recoverable(), &sink{}
	h.Trk.FetchErr = errors.New("linear boom")

	got := ship.Recover(h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if strings.Contains(calls(h), "push-force") {
		t.Errorf("calls = %v, want nothing pushed when the PR body can't be built", h.Calls)
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
			h, log := recoverable(), &sink{}
			tc.scene(h)

			got := ship.Recover(h, log, "PROJ-1")

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
	h, log := recoverable(), &sink{}
	h.FetchErr = errors.New("network down")

	if got := ship.Recover(h, log, "PROJ-1"); !got.Shipped {
		t.Fatalf("Recover() = %+v, want the fix shipped despite the fetch warning", got)
	}
	if !log.saw("could not fetch origin/main") {
		t.Errorf("events = %v, want the fetch failure warned", log.events)
	}
}
