package stages

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/hostio"
)

// #86: the review and retrospective sessions are handed the branch's base as the
// host resolved it, so they diff the ticket's own commits and not a stale local
// `main` that also carries already-merged sibling tickets.
func TestReviewAndRetrospectiveSessionsAreHandedTheBranchBase(t *testing.T) {
	for name, launch := range map[string]func(*hostio.Fake){
		"review": func(h *hostio.Fake) { Review(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"}) },
		"retrospective": func(h *hostio.Fake) {
			h.AgentFn = writesDropbox("[]")
			Retrospective(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})
		},
	} {
		h, repo := handedOff(t, "PROJ-1")
		base := repo.Git(repo.Dir, "rev-parse", "origin/main")
		// A sibling merges after the branch was cut: local main never sees it, and the
		// base stays the commit the branch was cut from.
		repo.Merge("other.txt", "unrelated\n", "a sibling merged")
		launch(h)
		if len(h.Agents) == 0 {
			t.Fatalf("%s launched no session", name)
		}
		if p := h.Agents[0].Prompt; !strings.Contains(p, "git diff "+base+"...HEAD") {
			t.Errorf("%s prompt does not scope the diff to the branch base %s", name, base)
		}
	}
}
