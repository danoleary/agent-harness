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
	const base = "f00dfeedf00dfeedf00dfeedf00dfeedf00dfeed"
	for name, launch := range map[string]func(*hostio.Fake){
		"review": func(h *hostio.Fake) { Review(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"}) },
		"retrospective": func(h *hostio.Fake) {
			h.AgentFn = writesDropbox("[]")
			Retrospective(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})
		},
	} {
		h := hostio.NewFake()
		h.Base = base
		launch(h)
		if len(h.Agents) == 0 {
			t.Fatalf("%s launched no session", name)
		}
		if p := h.Agents[0].Prompt; !strings.Contains(p, "git diff "+base+"...HEAD") {
			t.Errorf("%s prompt does not scope the diff to the branch base %s", name, base)
		}
	}
}
