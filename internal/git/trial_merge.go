package git

import (
	"fmt"
	"os"
)

// TrialMerge is a throwaway worktree holding a branch's HEAD with a ref merged
// in and not committed: what CI checks on a PR's merge ref, rebuilt locally so
// the gates can run over it before a push (#259). It is detached, so it holds no
// branch another worktree could need, and it sits beside the branch's own
// worktree, under the checkout, where the gate container's bind mount reaches.
type TrialMerge struct {
	path string
	repo string
	// Conflicts are the paths the merge left unmerged; none when it merged
	// cleanly. A trial merge with conflicts has nothing to gate.
	Conflicts []string
	runners
}

// Path is the host path of the trial merge's worktree, for the gate container.
func (m TrialMerge) Path() string { return m.path }

// TrialMerge merges ref into a detached copy of the worktree's HEAD, with
// `git merge --no-commit --no-ff`, in a scratch worktree at `<path>.merge`. The
// branch, its worktree and origin are untouched. A merge that conflicts is
// returned with its Conflicts named; git failing any other way is an error, and
// leaves no scratch worktree behind. One a run left behind is replaced. The
// caller removes it with [TrialMerge.Remove].
func (w Worktree) TrialMerge(ref string) (TrialMerge, error) {
	m := TrialMerge{path: w.path + ".merge", repo: w.repo, runners: w.runners}
	head, err := w.HeadSHA()
	if err != nil {
		return TrialMerge{}, fmt.Errorf("read %s's HEAD: %w", w.branch, err)
	}
	_ = m.Remove()
	if out, err := m.combined("git", "-C", w.repo, "worktree", "add", "-q", "--detach", m.path, head); err != nil {
		return TrialMerge{}, fmt.Errorf("add a scratch worktree at %s: %s", m.path, gitSaid(out, err))
	}
	args := append([]string{"-C", m.path}, identityArgs()...)
	args = append(args, "merge", "-q", "--no-commit", "--no-ff", ref)
	out, err := m.combined("git", args...)
	if err == nil {
		return m, nil
	}
	if unmerged, derr := m.output("git", "-C", m.path, "diff", "--name-only", "--diff-filter=U"); derr == nil {
		if m.Conflicts = pathLines(unmerged); len(m.Conflicts) > 0 {
			return m, nil
		}
	}
	_ = m.Remove()
	return TrialMerge{}, fmt.Errorf("merge %s into %s: %s", ref, shortSHA(head), gitSaid(out, err))
}

// Remove tears the trial merge's worktree down, forced: it holds an uncommitted
// merge and whatever the gates wrote, none of it anyone's work. One whose
// directory is already gone is pruned from git's records instead.
func (m TrialMerge) Remove() error {
	if _, err := os.Stat(m.path); os.IsNotExist(err) {
		return m.run("git", "-C", m.repo, "worktree", "prune")
	}
	return m.run("git", "-C", m.repo, "worktree", "remove", "--force", "--force", m.path)
}
