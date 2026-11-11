package git

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// ADRDir is where a repository keeps its numbered decision records,
// `NNNN-<slug>.md` (ADR-0034).
const ADRDir = "docs/adr"

// adrFile is an ADR's file name: its number and its slug.
var adrFile = regexp.MustCompile(`^([0-9][0-9][0-9][0-9])-(.+)\.md$`)

// Renumber is one of the branch's ADRs that a conflict resolution moved to
// another number, keeping its slug: the base had merged an ADR under the number
// the branch used (ADR-0034, #267).
type Renumber struct {
	// From and To are the old and new numbers, "0040" and "0041".
	From, To string
	// FromFile and ToFile are the file names under [ADRDir].
	FromFile, ToFile string
}

// RenumberedADRs compares the ADRs the branch added at head, before a conflict
// resolution, with the ones it adds now, at HEAD over its merge-base with
// origin/main. An ADR whose slug the branch still adds under another number was
// renumbered. Sorted by old number; none when every number stands.
func (w Worktree) RenumberedADRs(head string) ([]Renumber, error) {
	oldBase, err := w.output("git", "-C", w.path, "merge-base", BaseRef, head)
	if err != nil {
		return nil, err
	}
	newBase, err := w.output("git", "-C", w.path, "merge-base", BaseRef, "HEAD")
	if err != nil {
		return nil, err
	}
	before, err := w.addedADRs(strings.TrimSpace(string(oldBase)), head)
	if err != nil {
		return nil, err
	}
	after, err := w.addedADRs(strings.TrimSpace(string(newBase)), "HEAD")
	if err != nil {
		return nil, err
	}
	var out []Renumber
	for slug, was := range before {
		now, ok := after[slug]
		if !ok || now == was {
			continue
		}
		out = append(out, Renumber{
			From: adrFile.FindStringSubmatch(was)[1], To: adrFile.FindStringSubmatch(now)[1],
			FromFile: was, ToFile: now,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out, nil
}

// addedADRs is the ADR files from..to adds under [ADRDir], file name by slug.
func (w Worktree) addedADRs(from, to string) (map[string]string, error) {
	out, err := w.output("git", "-C", w.path, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=A", from, to, "--", ADRDir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, p := range nulPaths(out) {
		if path.Dir(p) != ADRDir {
			continue
		}
		if m := adrFile.FindStringSubmatch(path.Base(p)); m != nil {
			files[m[2]] = path.Base(p)
		}
	}
	return files, nil
}
