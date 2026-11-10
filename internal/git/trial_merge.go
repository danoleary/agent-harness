package git

import (
	"fmt"
	"os"
	"strings"
)

// TrialMerge is a throwaway worktree holding a branch's HEAD with a ref merged
// in and not committed: what CI checks on a PR's merge ref, rebuilt locally so
// the gates can run over it before a push (#259). It is detached, so it holds no
// branch another worktree could need, and it sits beside the branch's own
// worktree, under the checkout, where the gate container's bind mount reaches.
// [Worktree.TrialBase] builds one with nothing merged: the base alone.
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

// TrialBase checks commit out alone, detached, in a scratch worktree at
// `<path>.base`: the base with nothing of the branch in it, which the gates run
// over to tell a red base from a red branch (#250). Like a trial merge, it holds
// no branch and sits where the gate container's bind mount reaches. One a run
// left behind is replaced. The caller removes it with [TrialMerge.Remove].
func (w Worktree) TrialBase(commit string) (TrialMerge, error) {
	b := TrialMerge{path: w.path + ".base", repo: w.repo, runners: w.runners}
	_ = b.Remove()
	if out, err := b.combined("git", "-C", w.repo, "worktree", "add", "-q", "--detach", b.path, commit); err != nil {
		return TrialMerge{}, fmt.Errorf("add a scratch worktree at %s: %s", b.path, gitSaid(out, err))
	}
	return b, nil
}

// RefSHA is the commit ref names in the checkout, as of the last fetch.
func (w Worktree) RefSHA(ref string) (string, error) {
	out, err := w.output("git", "-C", w.repo, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", ref, err)
	}
	return strings.TrimSpace(string(out)), nil
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
