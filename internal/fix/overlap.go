package fix

import "strings"

// OverlapHeading heads a PR description's section for the overlap report a
// replay's conflict-resolution session wrote (#255).
const OverlapHeading = "## Overlap with `main`"

// OverlapSection is a PR description's section for overlaps: the heading, intro
// (what the reader is looking at, which depends on whether the PR was open when
// the replay ran), then one line per {file, main_added, branch_duplicates}; ""
// when there are none.
func OverlapSection(intro string, overlaps []Overlap) string {
	if len(overlaps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(OverlapHeading + "\n\n" + intro + "\n")
	for _, o := range overlaps {
		b.WriteString("\n- `" + oneLine(o.File) + "`: `main` added " + oneLine(o.MainAdded) + "; the branch duplicates it with " + oneLine(o.BranchDuplicates) + ".")
	}
	return b.String()
}

// OpenPROverlapIntro introduces the section a replay of an already-open PR puts
// in its description (#344): the PR loop's conflict fix, or the CI watch's
// reactive rebase.
const OpenPROverlapIntro = "While this PR was open, `main` gained code that the branch also implements in a mechanism of its own. The conflict-resolution session that brought the branch onto `main` reported these and left the branch's version in place. Nothing has reviewed them against the new base:"

// UpsertOverlapSection puts section in body in place of the overlap section an
// earlier replay left there, so a second replay replaces the report rather than
// stacking a copy of it (#344). A body with none gets it before the harness
// trailer (the last `---` rule), or at the end. An empty section leaves body as
// it is: a replay that reports nothing does not clear what an earlier one found.
func UpsertOverlapSection(body, section string) string {
	if section == "" {
		return body
	}
	if i := strings.Index(body, OverlapHeading); i >= 0 {
		rest := body[i+len(OverlapHeading):]
		end := len(rest)
		for _, stop := range []string{"\n## ", "\n---\n"} {
			if j := strings.Index(rest, stop); j >= 0 && j < end {
				end = j
			}
		}
		return body[:i] + section + "\n" + rest[end:]
	}
	if i := strings.LastIndex(body, "\n---\n"); i >= 0 {
		return strings.TrimRight(body[:i], "\n") + "\n\n" + section + "\n" + body[i:]
	}
	if strings.TrimSpace(body) == "" {
		return section + "\n"
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section + "\n"
}

// oneLine folds a session-written value onto one line, so it cannot break out of
// its list item.
func oneLine(s string) string {
	return strings.TrimSuffix(strings.Join(strings.Fields(s), " "), ".")
}
