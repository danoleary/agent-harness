package loophost

import (
	"errors"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/findings"
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
	if a := ticketArgs(item, claim); a.Selected != nowork.Of(issue330) {
		t.Errorf("Selected = %q, want the fingerprint of the selected ticket", a.Selected)
	}
	if a := ticketArgs(loop.WorkItem{Key: "#330"}, nil); a.Selected != "" {
		t.Errorf("an item with no selection context records nothing, got %q", a.Selected)
	}
}

// #453, replayed through the Source: the ticket's last run found it already
// resolved on main by a commit, and the ticket waits open for a human to close
// it. An unrelated merge moves main, and the next poll still passes over the
// ticket, because main still has the fix.
func TestNextPassesOverAResolvedTicketAfterAnUnrelatedMerge(t *testing.T) {
	repo := gittest.New(t)
	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = repo.Dir
	repo.Merge("helper.go", "func helper() {}\n", "PROJ-99: add the helper")
	fix := repo.OnOrigin("main")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}
	if err := nowork.For(cfg).WriteResolved("#330", fix, nowork.Of(issue330), fix, ""); err != nil {
		t.Fatal(err)
	}
	repo.Merge("docs.md", "an unrelated docs change\n", "Land a docs change")
	trk := newQueueTracker()
	trk.next, trk.nextOK = issue330, true
	log := &narrator{}
	s := NewTicketSource(cfg, log, trk)

	if _, claim, ok, err := s.Next(); ok || claim != nil || err != nil {
		t.Fatalf("Next() = (%v, %v, %v), want #330 passed over while main still has its fix", claim, ok, err)
	}
	if !saw(log, "skipping #330: already resolved on main by "+fix[:12]+"; awaiting close (#330)") {
		t.Errorf("events = %v, want the skip named", log.events)
	}
}

// Once the loop labels a resolved ticket blocked for a human, the label holds
// it, and removing the label is how a human says the commit does not fix it.
// Block keeps the record's verdict, marked held, so the screen dispatches the
// ticket once the label is gone and the run is told the verdict (#480). A label
// that could not be applied leaves the record to hold the ticket (#453).
func TestBlockingATicketForAHumanHoldsItsResolvedRecord(t *testing.T) {
	for name, c := range map[string]struct {
		blockErr error
		held     bool
	}{
		"labelled":      {held: true},
		"label refused": {blockErr: errors.New("no tracker.blocked_label is configured")},
	} {
		cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
		cfg.ProjectPath = t.TempDir()
		records := nowork.For(cfg)
		if err := records.WriteResolved("#330", "base", nowork.Of(issue330), "fix", "why"); err != nil {
			t.Fatal(err)
		}
		trk := newQueueTracker()
		trk.BlockErr = c.blockErr
		s := NewTicketSource(cfg, &narrator{}, trk)

		s.Block("#330")

		if rec, ok := records.Read("#330"); !ok || rec.ResolvedBy != "fix" || rec.Held != c.held {
			t.Errorf("%s: record = %+v, %v; want it kept, held = %v", name, rec, ok, c.held)
		}
	}
}

// #480, replayed through the Source: the ticket is a harness finding. Its run
// found it already resolved on main, and the loop could not label it blocked.
// Then a retrospective saw the finding recur and bumped the occurrence count in
// its body. That edit is the harness's own, so the next poll still passes over
// the ticket.
func TestNextPassesOverAResolvedTicketAfterTheHarnessBumpsItsOccurrences(t *testing.T) {
	repo := gittest.New(t)
	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = repo.Dir
	fix := repo.OnOrigin("main")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}
	if err := nowork.For(cfg).WriteResolved("#480", fix, nowork.Of(issue480), fix, "that commit already fixes it"); err != nil {
		t.Fatal(err)
	}
	trk := newQueueTracker()
	trk.BlockErr = errors.New("no tracker.blocked_label is configured to mark #480 with")
	s := NewTicketSource(cfg, &narrator{}, trk)
	s.Block("#480")
	bumped := issue480
	bumped.Description = findings.WithOccurrences(issue480.Description, 2)
	trk.next, trk.nextOK = bumped, true
	log := &narrator{}
	s = NewTicketSource(cfg, log, trk)

	if _, claim, ok, err := s.Next(); ok || claim != nil || err != nil {
		t.Fatalf("Next() = (%v, %v, %v), want #480 passed over after the harness's own edit", claim, ok, err)
	}
	if !saw(log, "skipping #480: already resolved on main by "+fix[:12]+"; awaiting close (#330)") {
		t.Errorf("events = %v, want the skip named", log.events)
	}
}

// issue480 is #480 as the retrospective filed it: a harness finding, carrying
// its dedup key.
var issue480 = ticket.Ticket{
	Identifier:  "#480",
	Title:       "Harness re-dispatched a ticket it had just marked already-resolved on main, with an identical prompt",
	Description: "**Kind:** harness-dispatch\n\nThe first implementation run ended with the harness recording the ticket as already resolved on main…\n\n_Surfaced during herd by the agent harness._\n\n<!-- finding-key: redispatch-after-already-fixed-handoff -->",
	Labels:      []string{"agent-ready", "harness-dispatch"},
}
