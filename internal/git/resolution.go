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

// SavedResolution is the resolution [Worktree.SaveResolution] saved for the branch
// at head, while it is still a resolution of the conflict the branch has: origin/main
// as the checkout last fetched it is in it, and it is not origin/main itself. One
// that origin/main has moved past was resolved onto a base that is gone, so it is
// dropped and reads as none. Any git failure reads as none: the caller then
// resolves afresh.
func (w Worktree) SavedResolution(head string) (string, bool) {
	out, err := w.output("git", "-C", w.repo, "rev-parse", "--verify", "-q", w.resolutionRefs()+head+"^{commit}")
	if err != nil {
		return "", false
	}
	tip := strings.TrimSpace(string(out))
	base, err := w.output("git", "-C", w.repo, "rev-parse", "--verify", "-q", BaseRef)
	if err != nil || strings.TrimSpace(string(base)) == tip ||
		w.run("git", "-C", w.repo, "merge-base", "--is-ancestor", BaseRef, tip) != nil {
		w.DropResolutions()
		return "", false
	}
	return tip, true
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
