package loop

import (
	"errors"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/loopstream"
)

// fixedNow is a stable clock for reaper tests so a claim's age is deterministic.
var fixedNow = time.Date(2026, 7, 3, 22, 0, 0, 0, time.UTC)

// reaperDeps builds the minimal Deps for exercising the between-ticket reaper: an
// empty queue (so the loop idles after the single reaper pass) and a stop arranged
// to land right after that first pass. released captures the ids the reaper released.
// The host is returned alongside so a test can script one more of its signals.
func reaperDeps(r *recorder, claims []StaleClaim, released *[]string) (Deps, *fakeHost) {
	h := &fakeHost{
		clearStop:     func() error { return nil },
		fetchMain:     func() error { return nil },
		stopRequested: stopAfter(1),
		resolveNext:   func() (string, bool) { return "", false },
		runPipeline:   func(string) TicketOutcome { return TicketOutcome{} },
		release:       func(id string) error { *released = append(*released, id); return nil },
		claims:        func() ([]StaleClaim, error) { return claims, nil },
	}
	return Deps{
		Host: h,
		Limits: Limits{
			ClaimTTL:     30 * time.Minute,
			PollInterval: time.Minute,
			TickInterval: 2 * time.Second,
		},
		Clock: testClock{now: func() time.Time { return fixedNow }, sleep: func(time.Duration) {}},
		Log:   r,
	}, h
}

// The tracer: an In Progress claim older than the TTL with no linked PR and no
// remote branch is stranded — the reaper releases it back to Todo within one loop
// pass and narrates the release for the viewer (AC-1).
func TestReapsStaleClaimPastTTL(t *testing.T) {
	r := &recorder{}
	var released []string
	claims := []StaleClaim{{Identifier: "BEH-451", StartedAt: fixedNow.Add(-40 * time.Minute)}}
	d, _ := reaperDeps(r, claims, &released)
	Run(d)

	if len(released) != 1 || released[0] != "BEH-451" {
		t.Fatalf("released = %v, want [BEH-451] (a stale no-branch/PR claim past the TTL)", released)
	}
	k, ok := r.kindFor("BEH-451")
	if !ok || k != loopstream.KindTicketReleased {
		t.Errorf("reap narration kind = %q (found=%v), want %q", k, ok, loopstream.KindTicketReleased)
	}
}

// The BEH-447 race regression (AC-2): a claim INSIDE the grace window looks
// identical to a dead one (no branch/PR yet) but is a healthy agent mid-work — it
// must never be reaped. 18 min < 30 min TTL, the exact observed mid-flight case.
func TestDoesNotReapClaimInsideGraceWindow(t *testing.T) {
	r := &recorder{}
	var released []string
	claims := []StaleClaim{{Identifier: "BEH-447", StartedAt: fixedNow.Add(-18 * time.Minute)}}
	d, _ := reaperDeps(r, claims, &released)
	Run(d)

	if len(released) != 0 {
		t.Fatalf("released = %v, want none — a claim inside the grace window must never be reaped", released)
	}
}

// A claim past the TTL but with a linked PR shipped (or is mid-review) — never reap.
func TestDoesNotReapClaimWithLinkedPR(t *testing.T) {
	r := &recorder{}
	var released []string
	claims := []StaleClaim{{Identifier: "BEH-633", StartedAt: fixedNow.Add(-2 * time.Hour), HasLinkedPR: true}}
	d, _ := reaperDeps(r, claims, &released)
	Run(d)

	if len(released) != 0 {
		t.Fatalf("released = %v, want none — a claim with a linked PR must never be reaped", released)
	}
}

// A claim past the TTL with no PR but a pushed branch has work in flight — never
// reap (the AND in the acceptance: no branch AND no PR).
func TestDoesNotReapClaimWithRemoteBranch(t *testing.T) {
	r := &recorder{}
	var released []string
	claims := []StaleClaim{{Identifier: "BEH-500", StartedAt: fixedNow.Add(-1 * time.Hour)}}
	d, h := reaperDeps(r, claims, &released)
	h.remoteBranch = func(id string) bool { return id == "BEH-500" }
	Run(d)

	if len(released) != 0 {
		t.Fatalf("released = %v, want none — a claim with a pushed branch must never be reaped", released)
	}
}

// Listing failures are non-fatal: a Linear hiccup during the reaper pass is warned
// and swallowed, and the daemon keeps running (it must never crash the loop).
func TestReaperListErrorIsNonFatal(t *testing.T) {
	r := &recorder{}
	var released []string
	d, h := reaperDeps(r, nil, &released)
	h.claims = func() ([]StaleClaim, error) { return nil, errors.New("linear boom") }
	code := Run(d)

	if code != 0 {
		t.Errorf("exit code = %d, want 0 — a reaper list error must not crash the daemon", code)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none on a list error", released)
	}
	if !r.saw("could not list In Progress claims") {
		t.Errorf("expected a warning about the failed claims list; events = %v", r.events)
	}
}

// A non-positive ClaimTTL is how reaping is switched off: the loop makes no reaper
// query and no release call at all. It is the ONLY off switch — the reaper used to
// disappear when the composition root left a func nil, which made "does this daemon
// reap?" a question only the wiring could answer; now it is a number an operator set.
func TestReaperDisabledByNonPositiveTTL(t *testing.T) {
	r := &recorder{}
	var released []string
	var listed int
	claims := []StaleClaim{{Identifier: "BEH-451", StartedAt: fixedNow.Add(-40 * time.Minute)}}

	d, h := reaperDeps(r, claims, &released)
	d.Limits.ClaimTTL = 0
	h.claims = func() ([]StaleClaim, error) { listed++; return claims, nil }
	Run(d)

	if listed != 0 {
		t.Errorf("listed claims %d×, want 0 — a disabled reaper must not query the tracker", listed)
	}
	if len(released) != 0 {
		t.Fatalf("released = %v, want none when ClaimTTL is non-positive", released)
	}
}
