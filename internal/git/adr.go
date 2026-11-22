package git

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// adrRef is an `ADR-NNNN` reference.
var adrRef = regexp.MustCompile(`\bADR-([0-9][0-9][0-9][0-9])\b`)

// RenumberRefs rewrites text's references to each renumbered ADR, `ADR-<old>`
// and its file name, to the new number, all at once, so a chain of renumbers
// (0040 → 0041 while 0041 → 0042) moves each reference once.
func RenumberRefs(text string, moved []Renumber) string {
	to := map[string]string{}
	var files []string
	for _, m := range moved {
		to[m.From] = m.To
		files = append(files, m.FromFile, m.ToFile)
	}
	text = strings.NewReplacer(files...).Replace(text)
	return adrRef.ReplaceAllStringFunc(text, func(ref string) string {
		if n, ok := to[adrRef.FindStringSubmatch(ref)[1]]; ok {
			return "ADR-" + n
		}
		return ref
	})
}

// RenumberCollidingADRs moves each ADR the replayed branch adds under a number
// origin/main now holds, or below main's highest, to the next free number (#374).
// A sibling that merged its own ADR under the branch's number after the gate
// leaves a replay that goes through clean, because the index unions (#235), and
// a tree with two ADRs on one number that check-adr fails on.
//
// The renumber goes into the commits that added the ADR, not into one on top
// (ADR-0034, #347): every commit on the branch is rebuilt over origin/main with
// the ADR's file renamed, and `ADR-<old>` and `<old>-<slug>.md` rewritten on every
// line the branch has that main's copy of the file does not, its index line and
// title included, and in its commit messages. Lines main already had keep their
// references, so a branch that cites main's decision under that number still
// does. It runs on the branch as the replay left it, on origin/main; a branch
// with a merge in its history is refused rather than flattened. On any failure
// the branch is put back on its tip. None when every number stands.
func (w Worktree) RenumberCollidingADRs() (moved []Renumber, err error) {
	main, err := w.output("git", "-C", w.path, "ls-tree", "--name-only", "-z", BaseRef+":"+ADRDir)
	if err != nil {
		return nil, nil // main keeps no ADRs: nothing to collide with
	}
	highest := 0
	for _, f := range nulPaths(main) {
		if m := adrFile.FindStringSubmatch(f); m != nil {
			n, _ := strconv.Atoi(m[1])
			highest = max(highest, n)
		}
	}
	added, err := w.addedADRs(BaseRef, "HEAD")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range added {
		files = append(files, f)
	}
	sort.Strings(files)
	next := highest + 1
	for _, f := range files {
		num := adrFile.FindStringSubmatch(f)
		n, _ := strconv.Atoi(num[1])
		if n >= next {
			next = n + 1
			continue
		}
		to := fmt.Sprintf("%04d", next)
		moved = append(moved, Renumber{From: num[1], To: to, FromFile: f, ToFile: to + "-" + num[2] + ".md"})
		next++
	}
	if len(moved) == 0 {
		return nil, nil
	}
	if merges, err := w.output("git", "-C", w.path, "rev-list", "--merges", BaseRef+"..HEAD"); err != nil {
		return nil, err
	} else if len(bytes.TrimSpace(merges)) > 0 {
		return nil, fmt.Errorf("the branch has a merge in its history; not rewriting it")
	}
	revs, err := w.output("git", "-C", w.path, "rev-list", "--reverse", BaseRef+"..HEAD")
	if err != nil {
		return nil, err
	}
	tip, err := w.HeadSHA()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = w.run("git", "-C", w.path, "reset", "-q", "--hard", tip)
			moved = nil
		}
	}()
	if err := w.run("git", "-C", w.path, "reset", "-q", "--hard", BaseRef); err != nil {
		return nil, err
	}
	for _, c := range strings.Fields(string(revs)) {
		if err := w.recommitRenumbered(c, moved); err != nil {
			return nil, fmt.Errorf("rebuilding %s: %w", shortSHA(c), err)
		}
	}
	return moved, nil
}

// recommitRenumbered commits c's tree on HEAD with moved applied to it: c's own
// author, date and message, the message renumbered too.
func (w Worktree) recommitRenumbered(c string, moved []Renumber) error {
	if err := w.run("git", "-C", w.path, "read-tree", "-u", "--reset", c); err != nil {
		return err
	}
	changed, err := w.output("git", "-C", w.path, "diff", "--name-only", "-z", "--no-renames", BaseRef, c)
	if err != nil {
		return err
	}
	for _, p := range nulPaths(changed) {
		if err := w.renumberFile(p, moved); err != nil {
			return err
		}
	}
	if err := w.run("git", "-C", w.path, "add", "-A"); err != nil {
		return err
	}
	meta, err := w.output("git", "-C", w.path, "log", "-1", "--format=%an <%ae>%x00%aI%x00%B", c)
	if err != nil {
		return err
	}
	parts := strings.SplitN(string(meta), "\x00", 3)
	if len(parts) != 3 {
		return fmt.Errorf("could not read commit %s", shortSHA(c))
	}
	args := append([]string{"-C", w.path}, identityArgs()...)
	args = append(args, "commit", "-q", "--no-verify", "--allow-empty", "--author="+parts[0], "--date="+parts[1], "-F", "-")
	if out, err := w.pipe([]byte(RenumberRefs(parts[2], moved)), "git", args...); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// renumberFile applies moved to p in the worktree: renames a renumbered ADR's
// file and rewrites the references on each line main's copy of p does not have.
// A binary file is left alone.
func (w Worktree) renumberFile(p string, moved []Renumber) error {
	src := filepath.Join(w.path, p)
	body, err := os.ReadFile(src)
	if os.IsNotExist(err) {
		return nil // c deleted it
	}
	if err != nil || bytes.IndexByte(body, 0) >= 0 {
		return err
	}
	onMain := map[string]bool{}
	if base, err := w.output("git", "-C", w.path, "show", BaseRef+":"+p); err == nil {
		for _, l := range strings.SplitAfter(string(base), "\n") {
			onMain[strings.TrimSuffix(l, "\n")] = true
		}
	}
	lines := strings.SplitAfter(string(body), "\n")
	for i, l := range lines {
		if !onMain[strings.TrimSuffix(l, "\n")] {
			lines[i] = RenumberRefs(l, moved)
		}
	}
	dst := src
	if path.Dir(p) == ADRDir {
		for _, m := range moved {
			if path.Base(p) == m.FromFile {
				dst = filepath.Join(w.path, ADRDir, m.ToFile)
			}
		}
	}
	if err := os.WriteFile(dst, []byte(strings.Join(lines, "")), 0o644); err != nil {
		return err
	}
	if dst != src {
		return os.Remove(src)
	}
	return nil
}
