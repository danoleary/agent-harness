package ship

import (
	"errors"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/hostio"
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

// --- Finish -----------------------------------------------------------------

// The tracer bullet: finishing a branch is force-with-lease + `gh pr create`, in
// that order, and it hands back the PR URL the caller narrates.
func TestFinishPushesForceWithLeaseThenOpensPR(t *testing.T) {
	h, log := hostio.NewFake(), &sink{}
	out := Finish(h, log, "proj-1", ticket.Ticket{Identifier: "PROJ-1", Title: "a ticket"})

	if !out.OK() {
		t.Fatalf("Finish() = %+v, want a pushed branch with an open PR", out)
	}
	if out.URL != h.PRURL {
		t.Errorf("URL = %q, want the created PR's URL %q", out.URL, h.PRURL)
	}
	if got := calls(h); !strings.Contains(got, "push-force proj-1 create-pr PROJ-1: a ticket") {
		t.Errorf("calls = %v, want a force-with-lease push then a PR create carrying the templated title", h.Calls)
	}
	if !log.saw("pushed feat/proj-1 to origin") {
		t.Errorf("events = %v, want the push narrated", log.events)
	}
}

// A failed push must not reach `gh pr create`: there is nothing on the remote to
// open a PR against, and the caller keeps the worktree.
func TestFinishStopsAtAFailedPush(t *testing.T) {
	h, log := hostio.NewFake(), &sink{}
	h.ForcePushErr = errors.New("remote rejected")

	out := Finish(h, log, "proj-1", ticket.Ticket{Identifier: "PROJ-1"})

	if out.Pushed || out.Err == nil {
		t.Fatalf("Finish() = %+v, want an unpushed branch carrying the push error", out)
	}
	if strings.Contains(calls(h), "create-pr") {
		t.Errorf("calls = %v, want no PR create after a failed push", h.Calls)
	}
}

// A push that lands and a `gh pr create` that then fails is the state the two
// dispositions differ on: the branch IS on the remote (so the caller must not
// report "nothing pushed"), but no PR exists.
func TestFinishReportsAPushedBranchWhenThePRCreateFails(t *testing.T) {
	h, log := hostio.NewFake(), &sink{}
	h.CreatePRErr = errors.New("gh: API rate limit exceeded")

	out := Finish(h, log, "proj-1", ticket.Ticket{Identifier: "PROJ-1"})

	if !out.Pushed {
		t.Errorf("Pushed = false, want true — the branch reached origin before gh failed")
	}
	if out.Err == nil || out.OK() {
		t.Fatalf("Finish() = %+v, want the gh error surfaced", out)
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

			if got := Recover(h, log, "PROJ-1"); got.Attempted || got.Shipped {
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

	got := Recover(h, log, "PROJ-1")

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

	got := Recover(h, log, "PROJ-1")

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

// emptyAfterRebase is a Host whose branch carries commits until the rebase
// replays it, then carries none — a sibling PR landed the same fix while the
// branch sat unpushed. `gh pr create` on that branch hard-fails ("No commits
// between main and feat/…"), so the recovery must stop before the push.
type emptyAfterRebase struct {
	*hostio.Fake
	rebased bool
}

func (h *emptyAfterRebase) Rebase(slug string) hostio.RebaseResult {
	h.rebased = true
	return h.Fake.Rebase(slug)
}

func (h *emptyAfterRebase) BranchDiffEmpty(slug string) bool { return h.rebased }

func TestRecoverStopsWhenTheRebaseEmptiesTheBranch(t *testing.T) {
	h, log := &emptyAfterRebase{Fake: recoverable()}, &sink{}

	got := Recover(h, log, "PROJ-1")

	if !got.Attempted || got.Shipped {
		t.Fatalf("Recover() = %+v, want attempted but not shipped", got)
	}
	if strings.Contains(calls(h.Fake), "push-force") {
		t.Errorf("calls = %v, want nothing pushed for a branch that became empty", h.Fake.Calls)
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

	got := Recover(h, log, "PROJ-1")

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

			got := Recover(h, log, "PROJ-1")

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

	if got := Recover(h, log, "PROJ-1"); !got.Shipped {
		t.Fatalf("Recover() = %+v, want the fix shipped despite the fetch warning", got)
	}
	if !log.saw("could not fetch origin/main") {
		t.Errorf("events = %v, want the fetch failure warned", log.events)
	}
}
