package stages

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// writesBlocked wraps a /tdd session so it also leaves marker as its blocked
// marker, and records the findings dir it ran with in dir.
func writesBlocked(t *testing.T, marker string, dir *string, then func(int, hostio.AgentRun) session.Outcome) func(int, hostio.AgentRun) session.Outcome {
	return func(call int, run hostio.AgentRun) session.Outcome {
		*dir = run.FindingsDir
		if err := os.WriteFile(filepath.Join(run.FindingsDir, BlockedMarker), []byte(marker), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if then == nil {
			return session.Outcome{}
		}
		return then(call, run)
	}
}

const blockedMarker = `{"reason": "the ticket's premise is false: the helper it asks to remove is still called by the PR loop", "questions": ["Keep the helper, or move the PR loop off it first?", "  "]}`

// #368, the incident: the session found the ticket's premise false, and, with no
// way to say so, recorded the block in a handoff commit. The harness read that as
// a handoff, reviewed it and re-queued the ticket, and the next session stopped on
// the same block. Now the blocked marker settles the run whatever the branch
// holds: SessionBlocked with the reason and questions, no tdd verdict, the claim
// kept for the loop to label the ticket blocked.
func TestImplementationRecordsASessionBlockedOnAHuman(t *testing.T) {
	for name, commits := range map[string]bool{
		"clean stop":                             false,
		"the block recorded in a handoff commit": true,
	} {
		t.Run(name, func(t *testing.T) {
			h, repo := fresh(t, "PROJ-40")
			var dir string
			var next func(int, hostio.AgentRun) session.Outcome
			if commits {
				next = commitsHandoff(h, repo)
			}
			h.AgentFn = writesBlocked(t, blockedMarker, &dir, next)

			log := stageLog(t, "PROJ-40")
			res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-40"})

			if res.Disposition != SessionBlocked || res.OK || res.Retryable {
				t.Fatalf("a session that stopped blocked on a human is a not-OK, not-retryable SessionBlocked, got %+v", res)
			}
			if !strings.Contains(res.BlockedWhy, "premise is false") {
				t.Errorf("BlockedWhy = %q, want the session's reason", res.BlockedWhy)
			}
			if want := []string{"Keep the helper, or move the PR loop off it first?"}; !reflect.DeepEqual(res.BlockedQuestions, want) {
				t.Errorf("BlockedQuestions = %q, want %q (blank entries dropped)", res.BlockedQuestions, want)
			}
			got := narration(t, log)
			if !strings.Contains(got, "tdd ⊘ blocked on a human") || strings.Contains(got, "tdd ✗") {
				t.Errorf("run.jsonl must log the block, not a tdd verdict; got %s", got)
			}
			if len(h.Trk.Released) != 0 {
				t.Errorf("released = %v, want the claim kept for the loop to hand to a human", h.Trk.Released)
			}
			if len(h.Filed) != 0 {
				t.Errorf("filed = %v, want nothing filed for a marker with no follow-ups", h.Filed)
			}
		})
	}
}

// #368: a ticket that says to split the work into two pull requests when a
// condition holds. The session ships the first half and names the second as a
// follow-up. The run is still the ordinary handoff, and the follow-up is filed
// through the findings path as a project finding that needs a human, so it lands
// linked to the worked ticket and without the ready label.
func TestImplementationFilesTheSessionsFollowUpsBehindAHuman(t *testing.T) {
	h, repo := fresh(t, "PROJ-41")
	h.Trk.Ticket.Identifier = "PROJ-41"
	var dir string
	marker := `{"follow_ups": [{"title": "Move the PR loop onto the new port", "body": "The second half of PROJ-41's split."}, {"title": "  ", "body": "no title"}]}`
	h.AgentFn = writesBlocked(t, marker, &dir, commitsHandoff(h, repo))

	log := stageLog(t, "PROJ-41")
	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-41"})

	if !res.OK || res.Disposition != NoPR {
		t.Fatalf("follow-ups alone do not block: the handoff stands, got %+v", res)
	}
	if want := []string{"PROJ-41"}; !reflect.DeepEqual(h.Filed, want) {
		t.Fatalf("filed = %v, want one filing linked to %v", h.Filed, want)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "out.json"))
	if err != nil {
		t.Fatalf("read the filed dropbox: %v", err)
	}
	parsed := findings.Parse(string(raw))
	if parsed.Error != "" || len(parsed.Findings) != 1 {
		t.Fatalf("filed findings = %+v, want the one titled follow-up", parsed)
	}
	f := parsed.Findings[0]
	if f.Title != "Move the PR loop onto the new port" || !f.NeedsHuman || f.Audience != findings.AudienceProject {
		t.Errorf("follow-up = %+v, want a project finding that needs a human, so it files not ready", f)
	}
	if !strings.Contains(f.Body, "The second half of PROJ-41's split.") || !strings.Contains(f.Body, "PROJ-41") {
		t.Errorf("follow-up body = %q, want the session's body and the worked ticket", f.Body)
	}
	if got := narration(t, log); strings.Contains(got, "tdd ✗") || !strings.Contains(got, "filing 1 follow-up ticket(s)") {
		t.Errorf("run.jsonl = %s, want the handoff verdict and the follow-up filing narrated", got)
	}
}

// A blocked session's follow-ups are filed too.
func TestImplementationFilesFollowUpsOfABlockedSession(t *testing.T) {
	h, _ := fresh(t, "PROJ-42")
	h.Trk.Ticket.Identifier = "PROJ-42"
	var dir string
	marker := `{"reason": "needs a decision", "follow_ups": [{"title": "Decide the port's shape", "body": "b"}]}`
	h.AgentFn = writesBlocked(t, marker, &dir, nil)

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-42"), Args{Identifier: "PROJ-42"})

	if res.Disposition != SessionBlocked {
		t.Fatalf("got %+v, want SessionBlocked", res)
	}
	if want := []string{"PROJ-42"}; !reflect.DeepEqual(h.Filed, want) {
		t.Errorf("filed = %v, want the follow-up filed linked to %v", h.Filed, want)
	}
}

// A marker that is not the documented JSON is narrated and read as no marker:
// the run keeps the verdict the ground truth gives it.
func TestImplementationIgnoresAMalformedBlockedMarker(t *testing.T) {
	h, _ := fresh(t, "PROJ-43")
	var dir string
	h.AgentFn = writesBlocked(t, `blocked: yes`, &dir, nil)

	log := stageLog(t, "PROJ-43")
	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-43"})

	if res.Disposition != NoPR || res.OK {
		t.Fatalf("a malformed marker blocks nothing: got %+v", res)
	}
	if got := narration(t, log); !strings.Contains(got, "blocked.json does not hold") || !strings.Contains(got, "tdd ✗ no handoff commit") {
		t.Errorf("run.jsonl = %s, want the unreadable marker narrated and the no-handoff failure kept", got)
	}
}

// The findings dir is keyed on the ticket, not the run, so a blocked marker an
// earlier run left must not block this one.
func TestImplementationIgnoresABlockedMarkerAnEarlierRunLeft(t *testing.T) {
	h, repo := fresh(t, "PROJ-44")
	var dir string
	h.AgentFn = writesBlocked(t, blockedMarker, &dir, nil)
	log := stageLog(t, "PROJ-44")
	if res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-44"}); res.Disposition != SessionBlocked {
		t.Fatalf("first run: got %+v", res)
	}

	h.AgentFn = commitsHandoff(h, repo)
	if res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-44"}); !res.OK || res.Disposition != NoPR {
		t.Errorf("a stale marker blocked the second run: got %+v", res)
	}
}
