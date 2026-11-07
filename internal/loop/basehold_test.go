package loop

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/stages"
)

// heldBase is the base SHA the held-base tests' red verdict was recorded for.
const heldBase = "3f9c2e7a1b04d5e6f708192a3b4c5d6e7f809112"

// steppedClock is a Clock whose sleeps advance its now, so a test can run the
// daemon through half an hour of polls in microseconds and see where the red
// verdict's expiry falls.
type steppedClock struct{ now time.Time }

func (c *steppedClock) Now() time.Time        { return c.now }
func (c *steppedClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

// stopAt stops the daemon once clock reaches until.
func stopAt(c *steppedClock, until time.Time) func() bool {
	return func() bool { return !c.now.Before(until) }
}

// heldRun is the Pipeline of a ticket held on a red base: like the implementation
// stage, it gives the claim back itself and reports the base it held on and when
// the red verdict expires.
func heldRun(t *testing.T, src *fakeSource, c *steppedClock, sha string) func(string) stages.Result {
	return func(string) stages.Result {
		if err := src.leased.Release(); err != nil {
			t.Fatalf("release: %v", err)
		}
		return stages.Result{Disposition: stages.BaseRed, HeldBase: sha, HeldUntil: c.now.Add(30 * time.Minute)}
	}
}

// #190, replayed: main was held red for 30 minutes with a one-minute poll and
// never moved. Before, every poll re-selected the same ticket, claimed it, cut and
// provisioned its worktree, read the cached red verdict and released it to Todo:
// about 26 tracker round trips and post_create runs, all certain to end in the
// same hold. Now the loop remembers the held base and the verdict's expiry, and a
// poll that finds origin/main unchanged idles with one heartbeat line, selecting
// nothing, until the verdict expires.
func TestRedBaseHoldIdlesWithoutClaimingUntilTheVerdictExpires(t *testing.T) {
	r := &recorder{}
	start := time.Date(2026, 11, 6, 14, 2, 0, 0, time.UTC)
	c := &steppedClock{now: start}
	var selected, released int
	src := &fakeSource{
		next:    func() (string, bool) { selected++; return "#171", true },
		release: func(string) error { released++; return nil },
	}
	code := Run(Deps{
		Host: &fakeHost{
			mainSHA:       func() (string, error) { return heldBase, nil },
			stopRequested: stopAt(c, start.Add(30*time.Minute+30*time.Second)),
		},
		Source:   src,
		Pipeline: &fakePipeline{run: heldRun(t, src, c, heldBase)},
		Limits:   Limits{PollInterval: time.Minute, TickInterval: 30 * time.Second, MaxConsecutiveFailures: 1},
		Clock:    c,
		Log:      r,
	})
	if code != 0 {
		t.Fatalf("code = %d, want a clean stop", code)
	}
	// Selected at 14:02 and, once the verdict expired, again at 14:32 — never in between.
	if selected != 2 || released != 2 {
		t.Errorf("selected %d / released %d times over 30 one-minute polls on an unchanged red base, want 2 / 2 (the first hold and the re-check once the verdict expired)", selected, released)
	}
	beats := 0
	for _, e := range r.events {
		if strings.Contains(e, "still held on 3f9c2e7a1b04, re-check at 14:32Z") {
			beats++
		}
	}
	if beats != 29 {
		t.Errorf("heartbeats = %d, want one per held poll (29); events = %v", beats, r.events)
	}
}

// The hold lasts only while origin/main sits on the held base: the first poll
// after a repair merges selects again, without waiting out the verdict.
func TestRedBaseHoldSelectsAgainOnceMainMoves(t *testing.T) {
	r := &recorder{}
	start := time.Date(2026, 11, 6, 14, 2, 0, 0, time.UTC)
	c := &steppedClock{now: start}
	var selected, fetches int
	src := &fakeSource{next: func() (string, bool) { selected++; return "#171", selected == 1 }}
	code := Run(Deps{
		Host: &fakeHost{
			fetchMain: func() error { fetches++; return nil },
			mainSHA: func() (string, error) {
				if c.now.Before(start.Add(5 * time.Minute)) {
					return heldBase, nil
				}
				return "c0ffee0000000000000000000000000000000000", nil // the repair merged
			},
			stopRequested: stopAt(c, start.Add(5*time.Minute+30*time.Second)),
		},
		Source:   src,
		Pipeline: &fakePipeline{run: heldRun(t, src, c, heldBase)},
		Limits:   Limits{PollInterval: time.Minute, TickInterval: 30 * time.Second},
		Clock:    c,
		Log:      r,
	})
	if code != 0 {
		t.Fatalf("code = %d, want a clean stop", code)
	}
	if selected != 2 {
		t.Errorf("selected %d times, want 2: the hold, then the first poll after main moved", selected)
	}
	// Startup, then one per poll with a hold recorded (14:03–14:07): the hold is
	// only as fresh as origin/main.
	if fetches != 6 {
		t.Errorf("fetched origin/main %d times, want 6 (startup + every poll with a hold recorded)", fetches)
	}
	if !r.saw("main moved") {
		t.Errorf("the loop must say why it selects again; events = %v", r.events)
	}
}

// A hold the loop cannot check — origin/main's SHA is unreadable — selects as
// before rather than idling on a record it cannot confirm.
func TestRedBaseHoldSelectsWhenMainCannotBeRead(t *testing.T) {
	r := &recorder{}
	start := time.Date(2026, 11, 6, 14, 2, 0, 0, time.UTC)
	c := &steppedClock{now: start}
	var selected int
	src := &fakeSource{next: func() (string, bool) { selected++; return "#171", true }}
	Run(Deps{
		Host: &fakeHost{
			mainSHA:       func() (string, error) { return "", errors.New("bad revision 'refs/remotes/origin/main'") },
			stopRequested: stopAt(c, start.Add(2*time.Minute+30*time.Second)),
		},
		Source:   src,
		Pipeline: &fakePipeline{run: heldRun(t, src, c, heldBase)},
		Limits:   Limits{PollInterval: time.Minute, TickInterval: 30 * time.Second},
		Clock:    c,
		Log:      r,
	})
	if selected != 3 {
		t.Errorf("selected %d times over 3 polls, want every poll when main cannot be read", selected)
	}
}
