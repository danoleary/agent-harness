package loophost

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/ticket"
)

var issue330 = ticket.Ticket{
	Identifier:  "#330",
	Title:       "Stop re-dispatching a ticket right after a run found it already resolved on main",
	Description: "What happened: the first implementation run on this ticket ended with no handoff commit…",
	Labels:      []string{"agent-ready", "stage-flow"},
}

// #330, replayed through the Source: the ticket's last run found nothing against
// the origin/main that is still current, the loop released it to Todo, and the
// next poll offers it again. Next passes over it instead of claiming it; once a
// sibling merge moves main, the next poll sees it without the loop fetching (the
// idle loop does not) and dispatches the ticket again.
func TestNextPassesOverATicketWhoseLastRunFoundNothingUntilMainMoves(t *testing.T) {
	repo := gittest.New(t)
	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = repo.Dir
	main, err := repo.Checkout().MainSHA()
	if err != nil {
		t.Fatal(err)
	}
	if err := nowork.For(cfg).Write("#330", main, nowork.Of(issue330)); err != nil {
		t.Fatal(err)
	}
	trk := newQueueTracker()
	trk.next, trk.nextOK = issue330, true
	log := &narrator{}
	s := NewTicketSource(cfg, log, trk)

	if _, claim, ok, err := s.Next(); ok || claim != nil || err != nil {
		t.Fatalf("Next() = (%v, %v, %v), want #330 passed over and nothing claimed", claim, ok, err)
	}
	if len(trk.Claimed) != 0 {
		t.Errorf("claimed %v, want nothing", trk.Claimed)
	}
	if !saw(log, "skipping #330: last run found no work against origin/main "+main[:12]+"; unchanged") {
		t.Errorf("events = %v, want the skip named", log.events)
	}
	if s.Held() == "" {
		t.Error("a queue the record holds is held, not empty (#281)")
	}

	repo.Merge("other.txt", "a sibling\n", "Land a sibling")
	if item, _, ok, err := s.Next(); !ok || err != nil || item.Key != "#330" {
		t.Fatalf("Next() = (%+v, %v, %v), want #330 dispatched once main moved", item, ok, err)
	}
}

// The item carries the ticket as selection read it, and the Pipeline hands that
// to the Stages, so a run that finds nothing records the same reading the next
// selection compares against.
func TestTheSelectedTicketReachesTheStageArgs(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = issue330, true
	s := sourceFor(t, config.Config{Project: config.Project{BranchPrefix: "feat"}}, trk)

	item, claim, ok, _ := s.Next()
	if !ok {
		t.Fatal("want #330 selected")
	}
	builtIn, _ := config.Project{}.LoopNamed("")
	if a := ticketArgs(builtIn, item, claim); a.Selected != nowork.Of(issue330) {
		t.Errorf("Selected = %q, want the fingerprint of the selected ticket", a.Selected)
	}
	if a := ticketArgs(builtIn, loop.WorkItem{Key: "#330"}, nil); a.Selected != "" {
		t.Errorf("an item with no selection context records nothing, got %q", a.Selected)
	}
}
