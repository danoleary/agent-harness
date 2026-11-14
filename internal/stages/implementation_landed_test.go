package stages

import (
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// #317's title, a body naming the file it asked to change, and #318's subject, as
// they were on 2026-11-14. #318 fixed #317 without naming it, so the own-key scan
// found nothing and #317 was dispatched six times, each session spending its turns
// finding the fix already on main (#335).
const (
	incident317Title   = "Make the entrypoint-trust tests pass on macOS, where the temp dir sits under the /var → /private/var symlink"
	incident317Body    = "`internal/sandbox/entrypoint_trust_test.go` looks up `/var/...` while the entrypoint writes `/private/var/...`. Resolve the temp workdir with `filepath.EvalSymlinks`."
	incident318Subject = "Resolve the temp workdir in the entrypoint trust tests, so they pass on macOS where /var is a symlink to /private/var (#318)"
	incident317File    = "internal/sandbox/entrypoint_trust_test.go"
)

// filed317 is #317 as the tracker returns it, filed an hour before the run.
func filed317() ticket.Ticket {
	return ticket.Ticket{Identifier: "#317", Title: incident317Title, Description: incident317Body, Created: time.Now().Add(-time.Hour)}
}

// The #317 replay: #318 lands on main after #317 is filed and changes the file
// #317 names. The run still dispatches (a commit touching a named file is not
// proof of a fix), but the run log warns and the session is told which commit to
// check before it writes any code.
func TestImplementationNamesACommitThatLandedOnAFileTheTicketNames(t *testing.T) {
	h, repo := fresh(t, "#317")
	h.Trk.Ticket = filed317()
	h.AgentFn = commitsHandoff(h, repo)
	repo.Merge(incident317File, "realTempDir\n", incident318Subject)
	log := stageLog(t, "#317")

	res := Implementation(h, stageCfg(), log, Args{Identifier: "#317"})

	if !res.OK || len(h.Agents) != 1 {
		t.Fatalf("the landed-commit advisory must never skip the dispatch, got %+v with runs %v", res, labels(h.Agents))
	}
	if got := narration(t, log); !strings.Contains(got, "⚠ #317 names files that changed on main since it was filed") || !strings.Contains(got, "(#318)") {
		t.Errorf("run log does not warn about #318:\n%s", got)
	}
	p := h.Agents[0].Prompt
	if !strings.Contains(p, incident318Subject) || !strings.Contains(p, incident317File) {
		t.Errorf("prompt does not name #318 and the file it changed:\n%s", p)
	}
	if !strings.Contains(p, "already fixed on main by") {
		t.Errorf("prompt does not tell the session how to stop when #318 is the fix:\n%s", p)
	}
}

// A commit that landed since the ticket was filed but changed none of the files it
// names says nothing: main moves all the time.
func TestImplementationIsQuietWhenNothingLandedOnAFileTheTicketNames(t *testing.T) {
	h, repo := fresh(t, "#317")
	h.Trk.Ticket = filed317()
	h.AgentFn = commitsHandoff(h, repo)
	repo.Merge("internal/loop/loop.go", "unrelated\n", "Tidy the loop (#319)")
	log := stageLog(t, "#317")

	Implementation(h, stageCfg(), log, Args{Identifier: "#317"})

	if got := narration(t, log); strings.Contains(got, "changed on main since it was filed") {
		t.Errorf("an unrelated commit raised the advisory:\n%s", got)
	}
	if p := h.Agents[0].Prompt; strings.Contains(p, "Tidy the loop") {
		t.Errorf("an unrelated commit reached the prompt:\n%s", p)
	}
}
