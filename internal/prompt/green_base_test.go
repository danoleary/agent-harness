package prompt

import (
	"strings"
	"testing"
)

// #278: a base-repair ticket whose base the host found green tells every
// Implement variant so, naming the SHA, and scopes the session to what the
// ticket asks for beyond the repair.
func TestImplementSaysTheBaseARepairTicketNamesIsGreen(t *testing.T) {
	const sha = "aa7a3c7279ea95ed5f6eba51a686c68c3224f227"
	for _, resume := range []Resume{Fresh, ResumedBranch, AfterRefusal} {
		p := For(Implement, Context{Ticket: sample, Slug: "gh-278", BranchPrefix: testPrefix, Body: exampleBody(t, "implement"), Resume: resume, GreenBase: sha})
		for _, want := range []string{"base gates are green at `" + sha + "`", "regression test"} {
			if !strings.Contains(p, want) {
				t.Errorf("resume %v: prompt missing %q", resume, want)
			}
		}
	}
}

func TestImplementOmitsTheGreenBaseSectionWhenUnset(t *testing.T) {
	if p := bImpl(t, sample, "gh-278"); strings.Contains(p, "base gates are green") {
		t.Error("a ticket the host did not find on a green base must carry no green-base line")
	}
}
