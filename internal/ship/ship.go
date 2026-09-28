// Package ship is the host-side "land a branch": refetch the base, replay the
// branch onto it, and push it with its pull request open (ADR-0002 — the harness
// owns every remote operation, the sandbox only ever runs `claude`).
//
// Three callers reach it, and before this module each carried its own copy of the
// sequence — copies that drifted. The review stage lands a branch its gate just
// passed; the daemon's committed-fix recovery lands a branch a *previous* run left
// committed, gate-green and unpushed (BEH-713); the post-PR CI watch re-replays a
// PR that went CONFLICTING against base (BEH-570). Only the review's copy had the
// disjoint-history guard (BEH-597); only it checked for a post-rebase collapse
// through verify. [Land] owns the whole sequence now, so a guard fixed here is fixed
// for every caller, and each caller maps one [Verdict] onto its own disposition.
//
// Everything here is expressed over [Port], a narrow slice of the host that
// hostio.Host satisfies, so the policy runs against hostio.Fake in a unit test and
// against docker/git/gh in production. Declaring the port here rather than taking
// hostio.Host is what lets hostio's CI watch reuse [Replay] without an import cycle.
package ship

import (
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/pr"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
	"github.com/danoleary/agent-harness/internal/verify"
)

// Port is the host surface landing a branch needs: the git reads and writes over
// the ticket's worktree, the origin fetch and push, `gh pr create`, and the tracker
// the committed-fix recovery builds its PR body from. hostio.Host satisfies it.
type Port interface {
	verify.GroundTruth

	BranchName(slug string) string
	CommitSubjects(slug string) []string
	Rebase(slug string) gitpkg.RebaseResult
	AbortRebase(slug string)
	IsDisjoint(slug string) bool

	FetchMain() error
	PushForceWithLease(slug string) error
	CreatePR(slug, title, body string) (string, error)
	OpenPRExists(slug string) bool

	Tracker() (tracker.Tracker, error)
}

// EventSink is the narration slice every caller can supply: the review stage's
// *runlog.Logger and the daemon's console narrator both satisfy it. Keeping it to
// the one method means ship never has to know which of the two it is talking to.
type EventSink interface {
	Event(string)
}

// Replayed is the verdict of the rebase half of a landing.
type Replayed int

const (
	// ReplayClean means the branch now sits on the latest base.
	ReplayClean Replayed = iota
	// ReplayConflict means a genuine content conflict; the branch is left on its
	// original tip.
	ReplayConflict
	// ReplayDisjoint means the branch shares no history with origin/main (BEH-597).
	// Its replay collides on every commit, so it is not a content conflict and no
	// resolution session can fix it; the branch is left on its original tip.
	ReplayDisjoint
)

// Replay is the rebase half of a landing: replay the branch onto the origin/main
// the caller has just fetched, and tell a disjoint history apart from a content
// conflict. On anything but a clean replay the worktree is restored to a clean,
// on-branch state (best-effort — git's own replay already restores on conflict), so
// a caller that stops here leaves nothing mid-replay behind.
//
// The fetch stays with the caller because the callers disagree on what a failed one
// means: a landing rebases onto the ref it has, while the CI watch refuses to
// re-push a branch replayed onto a base it could not refresh.
func Replay(h Port, slug string) Replayed {
	if h.Rebase(slug) == gitpkg.RebaseClean {
		return ReplayClean
	}
	h.AbortRebase(slug)
	if h.IsDisjoint(slug) {
		return ReplayDisjoint
	}
	return ReplayConflict
}

// Verdict is the one outcome of [Land]. Each caller maps it onto its own
// disposition; nothing else about the landing is theirs to decide.
type Verdict int

const (
	// Landed means the branch is pushed and its PR is open.
	Landed Verdict = iota
	// Collapsed means the replay left the branch with zero net change against
	// origin/main (a sibling PR landed the same fix — BEH-680). Nothing was pushed:
	// `gh pr create` would hard-fail with "No commits between main and feat/…".
	Collapsed
	// Conflict means the replay hit a content conflict that the caller's Resolve
	// hook did not resolve, or the caller had no hook. Nothing was pushed.
	Conflict
	// Disjoint means the branch has no common ancestor with origin/main (BEH-597).
	// Nothing was pushed, and Resolve was never called.
	Disjoint
	// PushFailed means the force-with-lease push was rejected. Nothing reached origin.
	PushFailed
	// PRFailed means the branch reached origin but `gh pr create` failed, so the
	// branch is pushed with no PR.
	PRFailed
)

// Landing is what [Land] did: the verdict, the PR's URL when Landed, and the git or
// gh error behind a PushFailed or PRFailed.
type Landing struct {
	Verdict Verdict
	URL     string
	Err     error
}

// Resolve is a caller's conflict hook: given a replay that hit a content conflict,
// it tries to leave the worktree cleanly rebased onto origin/main and reports
// whether it did. The review stage runs a sandboxed resolution session and re-runs
// its gate; a hook that returns false owns its own narration and cleanup.
type Resolve func() bool

// Land takes a clean, committed branch to an open pull request on the latest base:
// refetch origin/main, replay onto it, stop on a disjoint history, hand a content
// conflict to resolve (nil means abort), stop if the replay collapsed the branch to
// nothing, then push and open the PR from the ticket.
//
// The refetch is deliberate even when the caller fetched moments ago: a sibling PR
// can merge during a multi-minute gate, and replaying onto the pre-gate ref would
// still open a stale-base PR (BEH-570). A failed fetch is a warning — the replay
// goes onto the ref we have, and the CI watch's reactive rebase is the backstop.
func Land(h Port, log EventSink, slug string, t ticket.Ticket, resolve Resolve) Landing {
	if err := h.FetchMain(); err != nil {
		log.Event("ship … warning: could not fetch origin/main before rebase: " + err.Error())
	}
	switch Replay(h, slug) {
	case ReplayDisjoint:
		return Landing{Verdict: Disjoint}
	case ReplayConflict:
		if resolve == nil || !resolve() {
			return Landing{Verdict: Conflict}
		}
	}
	log.Event("rebased " + h.BranchName(slug) + " onto origin/main")

	// Re-check emptiness AFTER the replay (BEH-680): the caller's pre-replay check
	// ran on the stale tree, and a sibling PR that landed the same fix — or a
	// resolution session that dropped a now-empty commit — collapses the branch here.
	if verify.PostRebasePush(h, slug).RecommendClose {
		return Landing{Verdict: Collapsed}
	}
	return finish(h, log, slug, t)
}

// finish pushes the ticket's branch and opens its PR from the templated title and
// body (internal/pr) over the branch's commit subjects.
//
// The push is force-with-lease, never a plain push: the branch was just replayed
// onto the latest main, so on any branch already on the remote every SHA has been
// rewritten and a plain push is rejected non-fast-forward. That is not a rare
// state — a run whose `gh pr create` timed out after a successful push leaves
// exactly it, and because the ticket then has no PR it is re-selected, rebased
// again, and rejected again, forever. The lease keeps the force safe: it refuses
// to clobber remote commits the harness has not observed, and the harness owns
// this branch outright.
func finish(h Port, log EventSink, slug string, t ticket.Ticket) Landing {
	if err := h.PushForceWithLease(slug); err != nil {
		return Landing{Verdict: PushFailed, Err: err}
	}
	log.Event("pushed " + h.BranchName(slug) + " to origin")

	url, err := h.CreatePR(slug, pr.BuildTitle(t), pr.BuildBody(t, h.CommitSubjects(slug)))
	if err != nil {
		return Landing{Verdict: PRFailed, Err: err}
	}
	return Landing{Verdict: Landed, URL: url}
}
