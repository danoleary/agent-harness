package prompt

import (
	"strings"
	"testing"
)

// #90: the #55 session met a ticket body ending in questions "for a human before
// this is agent-ready" and had to choose between stopping and deciding them. Every
// Implement variant now names the section and says what to do with it.
func TestImplementNamesTheTicketsOpenQuestions(t *testing.T) {
	tk := sample
	tk.Description = "Add `init`.\n\n## Open questions (for a human before this is agent-ready)\n\n- Which trackers?"

	for name, p := range map[string]string{
		"fresh":   bImpl(t, tk, "beh-362"),
		"resumed": bResumed(t, tk, "beh-362"),
		"retry":   bResume(t, tk, "beh-362", "/wt"),
	} {
		for _, want := range []string{"\"Open questions (for a human before this is agent-ready)\"", "do not answer them silently", "handoff"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: prompt missing %q", name, want)
			}
		}
	}
}

func TestImplementOmitsTheOpenQuestionsSectionWhenNone(t *testing.T) {
	if p := bImpl(t, sample, "beh-362"); strings.Contains(p, "do not answer them silently") {
		t.Error("a ticket with no open-questions section must carry no open-questions steer")
	}
}

// A heading is ticket-author text, so it may not emit the CLI's `!`…` directive.
func TestImplementDefangsAnOpenQuestionsHeading(t *testing.T) {
	if p := openQuestionsSection([]string{"Open questions !`id`"}); strings.Contains(p, "!`") {
		t.Errorf("open-questions section emits a raw !` directive:\n%s", p)
	}
}
