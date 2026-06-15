package pr

import (
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/ticket"
)

var sample = ticket.Ticket{
	Identifier: "BEH-371",
	Title:      "Harness: review tool — cold review + push + PR",
	URL:        "https://linear.app/beherd/issue/BEH-371",
}

func TestBuildTitleCarriesTicketIDAndTitle(t *testing.T) {
	got := BuildTitle(sample)

	if !strings.Contains(got, "BEH-371") {
		t.Errorf("title %q missing the ticket id", got)
	}
	if !strings.Contains(got, sample.Title) {
		t.Errorf("title %q missing the ticket title", got)
	}
}

func TestBuildBodyListsCommitSubjects(t *testing.T) {
	subjects := []string{
		"feat(agent-harness): review tool (BEH-371)",
		"test(agent-harness): gate decision",
	}
	body := BuildBody(sample, subjects)

	for _, s := range subjects {
		if !strings.Contains(body, s) {
			t.Errorf("body missing commit subject %q", s)
		}
	}
}

func TestBuildBodyReferencesTicket(t *testing.T) {
	body := BuildBody(sample, []string{"feat: x"})

	if !strings.Contains(body, "BEH-371") {
		t.Errorf("body %q missing the ticket id", body)
	}
	if !strings.Contains(body, sample.URL) {
		t.Errorf("body %q missing the ticket URL", body)
	}
}

func TestBuildBodyCarriesGeneratedByTrailer(t *testing.T) {
	body := BuildBody(sample, []string{"feat: x"})

	// A trailer marking the PR as harness-generated (so a human reading the PR
	// knows it was not hand-authored).
	if !strings.Contains(strings.ToLower(body), "agent harness") {
		t.Errorf("body %q missing the generated-by trailer", body)
	}
}

func TestBuildBodyHandlesNoCommits(t *testing.T) {
	// Defensive: an empty subject list must still produce a usable body (the
	// harness should never crash on an unusual diff), not a stray empty bullet.
	body := BuildBody(sample, nil)

	if !strings.Contains(body, "BEH-371") {
		t.Errorf("body %q missing the ticket id even with no commits", body)
	}
	if strings.Contains(body, "\n- \n") {
		t.Errorf("body %q has an empty bullet", body)
	}
}
