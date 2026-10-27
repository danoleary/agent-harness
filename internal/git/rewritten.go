package git

import (
	"sort"
	"strings"
)

// Rewrite is how a replay changed the branch's own work (#114): what the branch
// held over its base before the replay, set against what it holds over its new
// base after. A clean cherry-pick moves the branch's changes, not their lines, so
// on a clean replay it is empty. A resolution session that ports a test,
// renumbers a file or rewords a commit shows up here.
type Rewrite struct {
	// Files are the paths whose changed lines differ between the two ranges, a path
	// the branch changes on only one side included. Sorted.
	Files []string
	// Messages reports whether the branch's commit messages differ.
	Messages bool
	// RangeDiff is `git range-diff` over the two ranges, what a reviewer reads. Its
	// creation factor is at the maximum, so a reworded or heavily edited commit is
	// still paired with the one it replaced and shown as a diff of the two, not as
	// one commit dropped and an unrelated one added.
	RangeDiff string
}

// RewrittenSince compares the branch as reviewed, base..head, with the branch now,
// its merge-base with origin/main up to HEAD. A file counts as rewritten when the
// lines the branch adds and removes in it differ. Context and line numbers do not
// count, so a change the base made next to the branch's is not a rewrite. Merge
// commits are left out of the message comparison: a resolution that merges the
// base in adds one without rewording the branch's own commits.
func (w Worktree) RewrittenSince(base, head string) (Rewrite, error) {
	newBase, err := w.output("git", "-C", w.path, "merge-base", BaseRef, "HEAD")
	if err != nil {
		return Rewrite{}, err
	}
	nb := strings.TrimSpace(string(newBase))

	before, err := w.changedPaths(base, head)
	if err != nil {
		return Rewrite{}, err
	}
	after, err := w.changedPaths(nb, "HEAD")
	if err != nil {
		return Rewrite{}, err
	}
	var r Rewrite
	for _, p := range unionPaths(before, after) {
		was, err := w.changedLines(base, head, p)
		if err != nil {
			return Rewrite{}, err
		}
		now, err := w.changedLines(nb, "HEAD", p)
		if err != nil {
			return Rewrite{}, err
		}
		if was != now {
			r.Files = append(r.Files, p)
		}
	}

	wasMsg, err := w.output("git", "-C", w.path, "log", "--no-merges", "--format=%B", base+".."+head)
	if err != nil {
		return Rewrite{}, err
	}
	nowMsg, err := w.output("git", "-C", w.path, "log", "--no-merges", "--format=%B", nb+"..HEAD")
	if err != nil {
		return Rewrite{}, err
	}
	r.Messages = strings.TrimSpace(string(wasMsg)) != strings.TrimSpace(string(nowMsg))

	rd, err := w.output("git", "-C", w.path, "range-diff", "--no-color", "--creation-factor=100", base+".."+head, nb+"..HEAD")
	if err != nil {
		return Rewrite{}, err
	}
	r.RangeDiff = strings.TrimRight(string(rd), "\n")
	return r, nil
}

// changedPaths lists the paths from..to changes, renames split into a delete and
// an add so a renumbered file shows under both names.
func (w Worktree) changedPaths(from, to string) ([]string, error) {
	out, err := w.output("git", "-C", w.path, "diff", "--name-only", "-z", "--no-renames", from, to)
	if err != nil {
		return nil, err
	}
	return nulPaths(out), nil
}

// changedLines is from..to's change to path reduced to the lines it adds and
// removes and its file-level markers (new, deleted, binary). Hunk headers carry
// line numbers and index lines carry blob ids, both of which a clean replay moves,
// so they are dropped.
func (w Worktree) changedLines(from, to, path string) (string, error) {
	out, err := w.output("git", "-C", w.path, "diff", "-U0", "--no-color", "--no-renames", "--no-ext-diff", from, to, "--", ":(literal)"+path)
	if err != nil {
		return "", err
	}
	var kept []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "index ") || strings.HasPrefix(line, "@@") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), nil
}

// nulPaths splits git's -z path output.
func nulPaths(out []byte) []string {
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// unionPaths is the sorted union of a and b.
func unionPaths(a, b []string) []string {
	seen := map[string]bool{}
	var all []string
	for _, p := range append(append([]string{}, a...), b...) {
		if !seen[p] {
			seen[p] = true
			all = append(all, p)
		}
	}
	sort.Strings(all)
	return all
}
