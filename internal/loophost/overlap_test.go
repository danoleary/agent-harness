package loophost

import (
	"errors"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// fakePRs is the repository's open PRs and the files each changes.
type fakePRs struct {
	open    []github.PullRequest
	files   map[int][]string
	listErr error
}

func (f *fakePRs) OpenPRs() ([]github.PullRequest, error) { return f.open, f.listErr }
func (f *fakePRs) Files(n int) ([]string, error)          { return f.files[n], nil }

func overlapConfig(dir string) config.Config {
	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = dir
	cfg.OverlapThreshold = 1
	cfg.OverlapEscapeLabel = "parallel-ok"
	return cfg
}

// A ready ticket that names a file an open harness PR changes is not claimed this
// poll, and the loop log names the PR and the file (#97).
func TestNextDefersATicketOverlappingAnOpenHarnessPR(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "#97", Description: "Tidy internal/loop/loop.go."}, true
	prs := &fakePRs{
		open:  []github.PullRequest{{Number: 81, HeadBranch: "feat/gh-80"}},
		files: map[int][]string{81: {"internal/loop/loop.go", "README.md"}},
	}
	log := &narrator{}
	s := NewTicketSource(overlapConfig(t.TempDir()), log, trk).WatchPRs(prs)

	if _, claim, ok, err := s.Next(); ok || claim != nil || err != nil {
		t.Fatalf("Next() = (%v, %v, %v), want an empty poll", claim, ok, err)
	}
	if len(trk.Claimed) != 0 {
		t.Errorf("claimed %v, want nothing", trk.Claimed)
	}
	if !saw(log, "#97 deferred: overlaps PR #81 on internal/loop/loop.go") {
		t.Errorf("events = %v, want the deferral named", log.events)
	}
}

// Only the harness's own PRs are its work in flight: a human's branch and a fork
// are not, so a ticket naming their files dispatches as today.
func TestNextIgnoresPRsOffTheBranchPrefix(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "#97", Description: "Tidy internal/loop/loop.go."}, true
	prs := &fakePRs{
		open: []github.PullRequest{
			{Number: 81, HeadBranch: "alice/refactor"},
			{Number: 82, HeadBranch: "feat/gh-1", Fork: true},
		},
		files: map[int][]string{81: {"internal/loop/loop.go"}, 82: {"internal/loop/loop.go"}},
	}
	s := NewTicketSource(overlapConfig(t.TempDir()), &narrator{}, trk).WatchPRs(prs)

	if item, _, ok, err := s.Next(); !ok || err != nil || item.Key != "#97" {
		t.Errorf("Next() = (%+v, %v, %v), want #97 dispatched", item, ok, err)
	}
}

// A claimed ticket's worktree is work in flight before its PR exists: what it has
// changed so far defers a ticket that names the same file.
func TestNextDefersATicketOverlappingAClaimedWorktree(t *testing.T) {
	repo := gittest.New(t)
	repo.Handoff("gh-5") // changes gh-5.txt
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{{Identifier: "#5"}}
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "#97", Description: "Follow up on gh-5.txt."}, true
	log := &narrator{}
	s := NewTicketSource(overlapConfig(repo.Dir), log, trk).WatchPRs(&fakePRs{})

	if _, _, ok, err := s.Next(); ok || err != nil {
		t.Fatalf("Next() = (%v, %v), want an empty poll", ok, err)
	}
	if !saw(log, "#97 deferred: overlaps #5 (claimed) on gh-5.txt") {
		t.Errorf("events = %v, want the deferral named", log.events)
	}
}

// A ticket another instance claimed a moment ago has no worktree edits yet, but
// what its own text names is the work it is about to do: a sibling naming the same
// file waits for it rather than running beside it.
func TestNextDefersASiblingOfAJustClaimedTicket(t *testing.T) {
	repo := gittest.New(t)
	trk := newQueueTracker()
	trk.Ticket = ticket.Ticket{Identifier: "#5", Description: "Reword README.md."}
	trk.claims = []tracker.InProgressClaim{{Identifier: "#5"}}
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "#97", Description: "Add a section to README.md."}, true
	log := &narrator{}
	s := NewTicketSource(overlapConfig(repo.Dir), log, trk).WatchPRs(&fakePRs{})

	if _, _, ok, err := s.Next(); ok || err != nil {
		t.Fatalf("Next() = (%v, %v), want an empty poll", ok, err)
	}
	if !saw(log, "#97 deferred: overlaps #5 (claimed) on README.md") {
		t.Errorf("events = %v, want the deferral named", log.events)
	}
}

// The PR list failing is narrated and the ticket dispatches: the check fails open.
func TestNextDispatchesWhenThePRsCannotBeRead(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "#97", Description: "Tidy internal/loop/loop.go."}, true
	log := &narrator{}
	s := NewTicketSource(overlapConfig(t.TempDir()), log, trk).WatchPRs(&fakePRs{listErr: errors.New("gh 502")})

	if _, _, ok, err := s.Next(); !ok || err != nil {
		t.Errorf("Next() = (%v, %v), want #97 dispatched", ok, err)
	}
	if !saw(log, "gh 502") {
		t.Errorf("events = %v, want the skipped check narrated", log.events)
	}
}

func saw(n *narrator, sub string) bool {
	for _, e := range n.events {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}
