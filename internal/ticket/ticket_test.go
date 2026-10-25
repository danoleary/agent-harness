package ticket

import "testing"

func TestIsLargeRefactor(t *testing.T) {
	cases := []struct {
		name string
		tk   Ticket
		want bool
	}{
		{
			// BEH-441: the canonical extract-and-rewire that overran a 30m cap
			// mid-surgery — extract N shared modules AND rewire N call sites to
			// consume them.
			name: "extract-and-rewire refactor",
			tk: Ticket{
				Title:       "Extract shared admin scaffolding: RovingTabBar, AddCategoryForm, CollapsibleCategoryHeader",
				Description: "Extract 5 shared modules, each with a unit test and story, THEN rewire 4 large manager files to consume them.",
			},
			want: true,
		},
		{
			// Extraction verb without any consume/rewire signal — a plain "pull this
			// out" ticket that is not the multi-call-site sequential shape.
			name: "extraction only",
			tk: Ticket{
				Title:       "Extract the media-permission classifier into its own helper",
				Description: "Move classifyMediaPermissionError into a standalone file and add a unit test.",
			},
			want: false,
		},
		{
			// Rewire/consume verb without any extraction — adopting an existing
			// primitive, not creating one.
			name: "rewire only",
			tk: Ticket{
				Title:       "Rewire the call dock to consume the new useCallToolSync hook",
				Description: "Point the dock at the already-shipped hook; no new module is created.",
			},
			want: false,
		},
		{
			// An ordinary guard/lint ticket — neither verb class present.
			name: "unrelated guard ticket",
			tk: Ticket{
				Title:       "Guard: flag exported components referenced only by their own storybook",
				Description: "Add a boundary lint rule and its tests.",
			},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tk.IsLargeRefactor(); got != c.want {
				t.Errorf("IsLargeRefactor() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"BEH-362":  "beh-362",
		"PROJ-7":   "proj-7",
		"#30":      "gh-30",
		" #30 ":    "gh-30",
		"ABC-1#2":  "abc-1-2",
		"Weird Id": "weird-id",
	}
	for key, want := range cases {
		if got := Slug(key); got != want {
			t.Errorf("Slug(%q) = %q, want %q", key, got, want)
		}
	}
}

// #90: #55 was dispatched with its body still ending in a section a human was
// meant to settle first. OpenQuestions names such sections so dispatch can warn
// and the envelope can tell the session not to answer them silently.
func TestOpenQuestions(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "the #55 heading",
			body: "Add `agent-harness init`.\n\n## Open questions (for a human before this is agent-ready)\n\n- Which trackers?\n",
			want: []string{"Open questions (for a human before this is agent-ready)"},
		},
		{
			name: "a not-agent-ready heading",
			body: "### Not yet agent-ready\n\nThe defaults need deciding.",
			want: []string{"Not yet agent-ready"},
		},
		{
			name: "a bold line standing in for a heading",
			body: "Do the thing.\n\n**Unresolved questions:**\n\n1. Which one?",
			want: []string{"Unresolved questions:"},
		},
		{
			name: "every such section, in order",
			body: "## Open question\n\nA?\n\n## Plan\n\nB.\n\n## Questions for a human\n\nC?",
			want: []string{"Open question", "Questions for a human"},
		},
		{
			name: "questions under sub-headings of the section",
			body: "## Open questions (for a human)\n\n### Trackers\n\nWhich?\n\n**Defaults**\n\nWhich?\n\n## Plan\n\nB.",
			want: []string{"Open questions (for a human)"},
		},
		{
			name: "a heading that says its questions are still to be decided",
			body: "## Open questions (to be decided by a human)\n\n- Which trackers?",
			want: []string{"Open questions (to be decided by a human)"},
		},
		{
			name: "a section that says it has none",
			body: "## Open questions\n\nNone.\n\n## Plan\n\nB.",
		},
		{
			name: "an empty section at the end",
			body: "Do it.\n\n## Open questions\n",
		},
		{
			name: "a heading marked resolved",
			body: "## Open questions (resolved)\n\n- Which trackers? GitHub and Linear.",
		},
		{
			name: "a heading inside a fenced block",
			body: "Template:\n\n```\n## Open questions\n\n- ?\n```\n",
		},
		{
			name: "the phrase in prose, not a heading",
			body: "There are no open questions left; this is agent-ready.",
		},
		{
			name: "a heading that says it is agent-ready",
			body: "## Why this is agent-ready\n\nEverything is decided.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Ticket{Description: c.body}.OpenQuestions()
			if len(got) != len(c.want) {
				t.Fatalf("OpenQuestions() = %q, want %q", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("OpenQuestions()[%d] = %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}
