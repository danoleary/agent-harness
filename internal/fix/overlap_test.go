package fix

import "testing"

// The open-PR paths put the report in a description that may already carry one
// (#344): an earlier replay's is replaced, not stacked, and the description's
// other sections and harness trailer are kept where they were.
func TestUpsertOverlapSectionReplacesRatherThanStacks(t *testing.T) {
	section := OverlapSection("Intro:", []Overlap{{File: "a.go", MainAdded: "X.", BranchDuplicates: "Y\nacross lines"}})
	const want = "## Overlap with `main`\n\nIntro:\n\n- `a.go`: `main` added X; the branch duplicates it with Y across lines."
	if section != want {
		t.Fatalf("OverlapSection = %q, want %q", section, want)
	}
	for name, c := range map[string]struct{ body, want string }{
		"appended before the trailer": {
			body: "Resolves #7.\n\n## Commits\n\n- c\n\n---\n\ntrailer\n",
			want: "Resolves #7.\n\n## Commits\n\n- c\n\n" + want + "\n\n---\n\ntrailer\n",
		},
		"replaces an earlier one before the trailer": {
			body: "Resolves #7.\n\n## Overlap with `main`\n\nOld:\n\n- `old.go`: stale.\n\n---\n\ntrailer\n",
			want: "Resolves #7.\n\n" + want + "\n\n---\n\ntrailer\n",
		},
		"replaces an earlier one before the next section": {
			body: "## Overlap with `main`\n\n- `old.go`: stale.\n\n## Notes\n\nkept\n",
			want: want + "\n\n## Notes\n\nkept\n",
		},
		"appended to a body with no trailer": {
			body: "A human's PR.\n",
			want: "A human's PR.\n\n" + want + "\n",
		},
		"an empty body": {body: "", want: want + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			got := UpsertOverlapSection(c.body, section)
			if got != c.want {
				t.Errorf("UpsertOverlapSection =\n%q\nwant\n%q", got, c.want)
			}
			if again := UpsertOverlapSection(got, section); again != got {
				t.Errorf("a second upsert of the same report changed the body:\n%q", again)
			}
		})
	}
	if got := UpsertOverlapSection("body", ""); got != "body" {
		t.Errorf("no report leaves the body alone, got %q", got)
	}
}
