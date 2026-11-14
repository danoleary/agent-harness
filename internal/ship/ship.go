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
// hostio.Host satisfies, so the policy runs against hostio.Fake over a temp repo in
// a unit test and against docker/git/gh in production. Declaring the port here rather than taking
// hostio.Host is what lets hostio's CI watch reuse [Replay] without an import cycle.
package ship

import (
	"strings"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/pr"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
	"github.com/danoleary/agent-harness/internal/verify"
)

// Port is the host surface landing a branch needs: the ticket's worktree (the git
// reads and the replay run on it directly), the origin fetch and push, `gh pr
// create`, and the tracker the committed-fix recovery builds its PR body from.
// hostio.Host satisfies it, bound to the one ticket being landed.
type Port interface {
	Worktree() gitpkg.Worktree

	FetchMain() error
	PushForceWithLease() error
	CreatePR(title, body string) (string, error)
	OpenPRExists() bool

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
	// ReplayConflict means a genuine content conflict: git left paths unmerged. The
	// branch is left on its original tip.
	ReplayConflict
	// ReplayDisjoint means the branch shares no history with origin/main (BEH-597).
	// Its replay collides on every commit, so it is not a content conflict and no
	// resolution session can fix it; the branch is left on its original tip.
	ReplayDisjoint
	// ReplayFailed means git failed with no content conflict (#164): a held lock, a
	// file in the way. It failed again on its one retry, or left the branch off its
	// original tip so no retry could run. There is nothing for a resolution session
	// to resolve.
	ReplayFailed
)

// Replay is the rebase half of a landing: replay the branch onto the origin/main
// the caller has just fetched, and tell a disjoint history and a git failure apart
// from a content conflict. On anything but a clean replay the worktree is restored
// to a clean, on-branch state (best-effort — git's own replay already restores on
// conflict), so a caller that stops here leaves nothing mid-replay behind. What git
// said when the replay did not apply goes to log.
//
// The fetch stays with the caller because the callers disagree on what a failed one
// means: a landing rebases onto the ref it has, while the CI watch refuses to
// re-push a branch replayed onto a base it could not refresh.
func Replay(h Port, log EventSink) Replayed {
	r, _ := replay(h, log)
	return r
}

// replay is [Replay] that also names the files git reported as conflicted, which
// a landing hands its Resolve hook (#114).
//
// A replay that failed with no content conflict is run once more before it is
// reported (#164). The failures behind it — an index.lock another git process
// held, a stat or permission blip on the bind-mounted worktree — are usually gone a
// moment later, and the alternative it used to get, a sandboxed resolution session
// and two extra gate runs, found nothing to resolve. The retry runs only when git
// put the branch back on the tip it started from: re-pinning any other tip would
// replay the wrong commits.
func replay(h Port, log EventSink) (Replayed, []string) {
	wt := h.Worktree()
	tip, _ := wt.HeadSHA()
	r := wt.RebaseDetail()
	if r.Result == gitpkg.RebaseClean {
		return ReplayClean, nil
	}
	wt.AbortRebase()
	if wt.IsDisjointFrom(gitpkg.BaseRef) {
		return ReplayDisjoint, nil
	}
	if !realConflict(r) {
		log.Event("ship … replay onto origin/main failed with no content conflict (" + describe(r) + "); git said:\n" + r.Output)
		if now, _ := wt.HeadSHA(); tip == "" || now != tip || !wt.Clean() {
			log.Event("ship ✗ the failed replay did not leave the branch on its tip " + tip + " — not retrying it (#164)")
			return ReplayFailed, nil
		}
		log.Event("ship ↻ retrying the replay once (#164)")
		if r = wt.RebaseDetail(); r.Result == gitpkg.RebaseClean {
			return ReplayClean, nil
		}
		wt.AbortRebase()
		if !realConflict(r) {
			log.Event("ship ✗ replay onto origin/main failed again with no content conflict (" + describe(r) + "); git said:\n" + r.Output)
			return ReplayFailed, nil
		}
	}
	log.Event("ship · replay onto origin/main hit a content conflict in " + strings.Join(r.Conflicted, ", ") + "; git said:\n" + r.Output)
	return ReplayConflict, r.Conflicted
}

// realConflict reports whether a replay stopped on a content conflict a resolution
// session can act on: git said so, and named the paths it left unmerged.
func realConflict(r gitpkg.RebaseReport) bool {
	return r.Result == gitpkg.RebaseConflict && len(r.Conflicted) > 0
}

// describe says which kind of stop a replay report is, for the narration.
func describe(r gitpkg.RebaseReport) string {
	if r.Result == gitpkg.RebaseConflict {
		return "git reported a conflict but named no unmerged paths"
	}
	return r.Result.String()
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
	// RebaseFailed means git failed to replay the branch with no content conflict,
	// and the replay was not rescued by its retry (#164). Nothing was pushed, and Resolve was never called.
	RebaseFailed
	// PushFailed means the force-with-lease push was rejected. Nothing reached origin.
	PushFailed
	// PRFailed means the branch reached origin but `gh pr create` failed, so the
	// branch is pushed with no PR.
	PRFailed
)

// Landing is what [Land] did: the verdict, the PR's URL, title and body when
// Landed, and the git or gh error behind a PushFailed or PRFailed. The title and
// body are what a later edit of the open PR starts from (#344).
type Landing struct {
	Verdict     Verdict
	URL         string
	Title, Body string
	Err         error
}

// Resolve is a caller's conflict hook: given a replay that hit a content conflict
// in the conflicted files git named, it tries to leave the worktree cleanly
// rebased onto origin/main and reports whether it did. The review stage runs a
// sandboxed resolution session, re-runs its gate, and reviews what the session
// wrote beyond those files (#114); a hook that fails owns its own narration and
// cleanup.
type Resolve func(conflicted []string) Resolution

// Resolution is what a [Resolve] hook did. OK means the worktree is rebased onto
// origin/main and cleared to push. Note, when set, goes into the PR body: what the
// resolution wrote after the branch was reviewed, for the human who reads the PR.
type Resolution struct {
	OK   bool
	Note string
}

// Opening is the pull request a landing opens: its title, and its body built from
// the branch's commit subjects (as the replay left them) and the conflict hook's
// note. A ticket's comes from the tracker ([Land]); a security-alerts landing has
// no ticket and describes its own (ADR-0040).
type Opening struct {
	Title string
	Body  func(subjects []string, note string) string
}

// Land is [LandAs] with the ticket's PR: the templated title and body
// (internal/pr).
func Land(h Port, log EventSink, t ticket.Ticket, resolve Resolve) Landing {
	return LandAs(h, log, Opening{
		Title: pr.BuildTitle(t),
		Body:  func(subjects []string, note string) string { return pr.BuildBody(t, subjects, note) },
	}, resolve)
}

// LandAs takes a clean, committed branch to an open pull request on the latest
// base: refetch origin/main, replay onto it, stop on a disjoint history or a replay
// git could not run, hand a content conflict to resolve (nil means abort), stop if
// the replay collapsed the branch to nothing, then push and open the PR as opening
// describes it.
//
// The refetch is deliberate even when the caller fetched moments ago: a sibling PR
// can merge during a multi-minute gate, and replaying onto the pre-gate ref would
// still open a stale-base PR (BEH-570). A failed fetch is a warning — the replay
// goes onto the ref we have, and the CI watch's reactive rebase is the backstop.
func LandAs(h Port, log EventSink, opening Opening, resolve Resolve) Landing {
	if err := h.FetchMain(); err != nil {
		log.Event("ship … warning: could not fetch origin/main before rebase: " + err.Error())
	}
	var note string
	switch replayed, conflicted := replay(h, log); replayed {
	case ReplayDisjoint:
		return Landing{Verdict: Disjoint}
	case ReplayFailed:
		return Landing{Verdict: RebaseFailed}
	case ReplayConflict:
		if resolve == nil {
			return Landing{Verdict: Conflict}
		}
		r := resolve(conflicted)
		if !r.OK {
			return Landing{Verdict: Conflict}
		}
		note = r.Note
	}
	log.Event("rebased " + h.Worktree().Branch() + " onto origin/main")

	// Re-check emptiness AFTER the replay (BEH-680): the caller's pre-replay check
	// ran on the stale tree, and a sibling PR that landed the same fix — or a
	// resolution session that dropped a now-empty commit — collapses the branch here.
	if verify.PostRebasePush(h.Worktree()).RecommendClose {
		return Landing{Verdict: Collapsed}
	}
	return finish(h, log, opening, note)
}

// finish pushes the branch and opens its PR from opening, over the branch's commit
// subjects and the conflict hook's note.
//
// The push is force-with-lease, never a plain push: the branch was just replayed
// onto the latest main, so on any branch already on the remote every SHA has been
// rewritten and a plain push is rejected non-fast-forward. That is not a rare
// state — a run whose `gh pr create` timed out after a successful push leaves
// exactly it, and because the ticket then has no PR it is re-selected, rebased
// again, and rejected again, forever. The lease keeps the force safe: it refuses
// to clobber remote commits the harness has not observed, and the harness owns
// this branch outright.
func finish(h Port, log EventSink, opening Opening, note string) Landing {
	if err := h.PushForceWithLease(); err != nil {
		return Landing{Verdict: PushFailed, Err: err}
	}
	log.Event("pushed " + h.Worktree().Branch() + " to origin")

	body := opening.Body(h.Worktree().CommitSubjects(), note)
	url, err := h.CreatePR(opening.Title, body)
	if err != nil {
		return Landing{Verdict: PRFailed, Err: err}
	}
	return Landing{Verdict: Landed, URL: url, Title: opening.Title, Body: body}
}
