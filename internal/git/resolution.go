package git

import (
	"path/filepath"
	"strings"
)

// resolutionRefs is the namespace a slug's saved conflict resolutions live under:
// `refs/harness/resolved/<slug>/<head>`, one per branch head they resolve (#252).
// The ref keeps the resolved commits reachable after the worktree is removed and
// the branch is reset to origin's tip; its name records which head was resolved,
// since a rebase leaves no trace of that head in the commits themselves.
func (w Worktree) resolutionRefs() string {
	return "refs/harness/resolved/" + filepath.Base(w.path) + "/"
}

// SaveResolution saves the worktree's HEAD as the committed conflict resolution
// of the branch at head (#252), replacing any resolution saved for another head.
// The caller saves one the ground truth accepted but whose push the gates held
// back, so the next run at the same head can re-gate it instead of resolving the
// conflict again. It pushes nothing.
func (w Worktree) SaveResolution(head string) (string, error) {
	tip, err := w.HeadSHA()
	if err != nil {
		return "", err
	}
	w.DropResolutions()
	if err := w.run("git", "-C", w.repo, "update-ref", w.resolutionRefs()+head, tip); err != nil {
		return "", err
	}
	return tip, nil
}

// Saved is a resolution [Worktree.SaveResolution] saved: Tip is the resolved
// commit, and Base the origin/main it was resolved onto. Stale marks one cut from
// an origin/main the checkout has since fetched past (#479): it no longer resolves
// the conflict the branch has against origin/main as it stands, but its resolved
// commits can still be replayed onto it ([Worktree.ReplayResolution]).
type Saved struct {
	Tip   string
	Base  string
	Stale bool
}

// SavedResolution is the resolution [Worktree.SaveResolution] saved for the branch
// at head, and whether there is one. A resolution origin/main as the checkout last
// fetched it is in resolves the conflict the branch has now. One that origin/main
// has moved past is Stale, and kept: its resolved hunks are still the ones the
// branch's conflict needs, so the caller replays it onto the new base before
// resolving afresh (#479). One that is origin/main itself resolves nothing, so it
// is dropped and reads as none. Any git failure reads as none: the caller then
// resolves afresh.
func (w Worktree) SavedResolution(head string) (Saved, bool) {
	out, err := w.output("git", "-C", w.repo, "rev-parse", "--verify", "-q", w.resolutionRefs()+head+"^{commit}")
	if err != nil {
		return Saved{}, false
	}
	tip := strings.TrimSpace(string(out))
	main, err := w.output("git", "-C", w.repo, "rev-parse", "--verify", "-q", BaseRef)
	if err != nil {
		return Saved{}, false
	}
	if strings.TrimSpace(string(main)) == tip {
		w.DropResolutions()
		return Saved{}, false
	}
	if w.run("git", "-C", w.repo, "merge-base", "--is-ancestor", BaseRef, tip) == nil {
		return Saved{Tip: tip, Base: strings.TrimSpace(string(main))}, true
	}
	base, err := w.output("git", "-C", w.repo, "merge-base", tip, BaseRef)
	if err != nil {
		return Saved{}, false
	}
	return Saved{Tip: tip, Base: strings.TrimSpace(string(base)), Stale: true}, true
}

// ReplayResolution replays a stale saved resolution, tip, onto origin/main as it
// stands (#479), as a conflict-resolution session would replay the branch: a
// rebase replays the resolved commits origin/main does not have onto it, as
// [Worktree.RebaseDetail] does; merge merges origin/main into tip, as a human's
// branch is resolved. The conflict the saved resolution settled is already settled
// in its commits, so only what origin/main gained since its base can stop git. On
// a clean replay the branch sits at the result; on any other the branch is put
// back on the tip it started from, and the report says what git said.
func (w Worktree) ReplayResolution(tip string, merge bool) RebaseReport {
	orig, err := w.HeadSHA()
	if err != nil {
		return RebaseReport{Result: RebaseFailed, Output: err.Error()}
	}
	if err := w.ResumeResolution(tip); err != nil {
		_ = w.ResumeResolution(orig)
		return RebaseReport{Result: RebaseFailed, Output: err.Error()}
	}
	var r RebaseReport
	if merge {
		r = w.mergeBase()
	} else {
		r = w.rebase(true)
	}
	if r.Result != RebaseClean {
		_ = w.ResumeResolution(orig)
	}
	return r
}

// mergeBase merges origin/main into the worktree's HEAD and commits the merge.
// One git stops on is aborted, with the paths it left unmerged named.
func (w Worktree) mergeBase() RebaseReport {
	args := append([]string{"-C", w.path}, identityArgs()...)
	args = append(args, "merge", "-q", "--no-ff", "--no-edit", BaseRef)
	out, err := w.combined("git", args...)
	if err == nil {
		return RebaseReport{Result: RebaseClean}
	}
	r := RebaseReport{Result: RebaseFailed, Output: gitSaid(out, err)}
	if unmerged, derr := w.output("git", "-C", w.path, "diff", "--name-only", "-z", "--diff-filter=U"); derr == nil {
		if r.Conflicted = nulPaths(unmerged); len(r.Conflicted) > 0 {
			r.Result = RebaseConflict
		}
	}
	_ = w.run("git", "-C", w.path, "merge", "--abort")
	return r
}

// ResumeResolution moves the worktree's branch to a saved resolution, tip, as if
// the session that made it had just finished.
func (w Worktree) ResumeResolution(tip string) error {
	return w.run("git", "-C", w.path, "reset", "-q", "--hard", tip)
}

// DropResolutions deletes every resolution saved for the slug: one that was
// pushed, or one that no longer resolves anything. Best-effort.
func (w Worktree) DropResolutions() {
	out, err := w.output("git", "-C", w.repo, "for-each-ref", "--format=%(refname)", w.resolutionRefs())
	if err != nil {
		return
	}
	for _, ref := range strings.Fields(string(out)) {
		_ = w.run("git", "-C", w.repo, "update-ref", "-d", ref)
	}
}
