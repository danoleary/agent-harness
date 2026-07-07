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
