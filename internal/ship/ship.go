// Package ship is the host-side "finish a branch": push it and open its pull
// request (ADR-0002 — the harness owns every remote operation, the sandbox only
// ever runs `claude`).
//
// Two callers reach it, and before this package they each carried their own copy
// of it. The review stage finishes a branch its gate just passed; the daemon's
// committed-fix recovery finishes a branch a *previous* run left committed,
// gate-green and unpushed (BEH-713). The second copy lived in `package main`,
// where nothing could import it — 60 lines of ship-critical policy, nine git/gh
// calls and six early returns deciding whether a ticket ships, reachable from a
// test only through a live Docker daemon, a live tracker and a live GitHub.
//
// Everything here is expressed over [hostio.Host], so the policy runs against
// [hostio.Fake] in a unit test and against docker/git/gh in production.
package ship

import (
	"strings"

	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/pr"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// EventSink is the narration slice both callers can supply: the review stage's
// *runlog.Logger and the daemon's console narrator both satisfy it. Keeping it to
// the one method means ship never has to know which of the two it is talking to.
type EventSink interface {
	Event(string)
}

// Outcome is the result of finishing a branch. Pushed and URL are cumulative
// progress markers, not alternatives: a push that succeeded and a `gh pr create`
// that then failed leaves Pushed true, URL empty and Err set — the state a caller
// must narrate differently ("branch pushed, open the PR manually") from a failed
// push ("keeping worktree, nothing pushed").
type Outcome struct {
	// Pushed is true once the branch reached origin.
	Pushed bool
	// URL is the created pull request's URL, empty unless Err is nil.
	URL string
	// Err is the first failure, or nil when the PR is open.
	Err error
}

// OK reports whether the branch is pushed and its PR is open.
func (o Outcome) OK() bool { return o.Pushed && o.Err == nil }

// Finish pushes the ticket's branch and opens its PR from the templated title and
// body (internal/pr) over the branch's commit subjects.
//
// The push is force-with-lease, never a plain push: both callers rebase onto the
// latest main first, so on any branch already on the remote every SHA has just
// been rewritten and a plain push is rejected non-fast-forward. That is not a rare
// state — a run whose `gh pr create` timed out after a successful push leaves
// exactly it, and because the ticket then has no PR it is re-selected, rebased
// again, and rejected again, forever. The lease keeps the force safe: it refuses
// to clobber remote commits the harness has not observed, and the harness owns
// this branch outright.
func Finish(h hostio.Host, log EventSink, slug string, t ticket.Ticket) Outcome {
	if err := h.PushForceWithLease(slug); err != nil {
		return Outcome{Err: err}
	}
	log.Event("pushed " + h.BranchName(slug) + " to origin")

	url, err := h.CreatePR(slug, pr.BuildTitle(t), pr.BuildBody(t, h.CommitSubjects(slug)))
	if err != nil {
		return Outcome{Pushed: true, Err: err}
	}
	return Outcome{Pushed: true, URL: url}
}

// Recovery is what the committed-fix recovery learned about one ticket.
// Attempted is the "was there anything to finish?" signal the daemon branches on:
// false means this was a genuine no-PR run (an OOM, a crash, an empty diff) that
// should be released to Todo exactly as before, so a recovery that finds nothing
// is never mistaken for one that failed.
type Recovery struct {
	// Attempted is true only when the branch really was in the committed-but-unshipped
	// state and the recovery ran.
	Attempted bool
	// Shipped is true when the branch is now pushed with an open PR.
	Shipped bool
}

// Recover finishes a no-PR run whose fix was already committed on its branch but
// never pushed or PR'd — the recovered-checkpoint / verify-only state where the
// review stage left a gate-green, clean commit yet never reached a PR (BEH-713,
// the BEH-649 infinite Todo↔In-Progress bounce). Releasing such a ticket just
// re-grabs it to the same no-verdict conclusion forever, so before the daemon
// treats a no-PR run as a failure it tries to finish it here.
//
// It reports Attempted=true ONLY when there was such a state to finish: a worktree
// on disk, clean, carrying commits against origin/main, with no open PR. Every
// other shape returns the zero Recovery and the daemon releases as before.
//
// On a recoverable branch it rebases onto the latest main (so the PR opens on a
// current base) and hands off to [Finish]. Any git/gh/tracker failure returns
// Attempted=true with Shipped=false, so the daemon falls through to its normal
// release and a later run re-grabs it — an attempt is never worse than no attempt.
// Opening a PR is safe recovery, not a merge: CI and human review still gate that.
func Recover(h hostio.Host, log EventSink, key string) Recovery {
	slug := strings.ToLower(key)

	// A committed fix means a worktree that is there and clean: uncommitted edits are
	// an in-progress or crashed run, not a finished-but-unpushed one, so those are
	// left to the normal release path.
	if !h.WorktreeExists(slug) || !h.WorktreeClean(slug) {
		return Recovery{}
	}
	// Refresh main so both the empty-diff check and the PR base are current.
	if err := h.FetchMain(); err != nil {
		log.Event("loop … warning: could not fetch origin/main during committed-fix recovery: " + err.Error())
	}
	// Nothing committed ahead of main (already merged, or an empty branch) → not a
	// recoverable committed fix.
	if h.BranchDiffEmpty(slug) {
		return Recovery{}
	}
	// A branch that already has an OPEN PR isn't stranded (its outcome would already
	// carry ReachedPushedPR — belt-and-braces), so there is nothing to finish.
	if h.OpenPRExists(slug) {
		return Recovery{}
	}

	log.Event("loop — " + key + " has a committed, unpushed fix with no PR; completing it host-side (rebase + push + PR) (BEH-713)")

	// Rebase onto the latest main so the PR opens on a current base. A genuine content
	// conflict can't be auto-completed — attempted, but not shipped, so the daemon
	// releases it for a later run or a human to resolve (never a worse state).
	if h.Rebase(slug) == hostio.RebaseConflict {
		h.AbortRebase(slug)
		log.Event("loop … " + key + " committed-fix recovery: rebase onto main conflicted — cannot auto-complete")
		return Recovery{Attempted: true}
	}
	// The rebase can collapse the branch to zero net change (a sibling PR landed the
	// same fix) — nothing to open a PR for.
	if h.BranchDiffEmpty(slug) {
		log.Event("loop … " + key + " committed-fix recovery: branch became empty after rebase — nothing to ship")
		return Recovery{Attempted: true}
	}
	// The ticket is fetched BEFORE the push, not after: the PR body needs it, and a
	// tracker hiccup should leave the branch exactly as it was rather than pushed with
	// no PR (which is the very state this function exists to clean up).
	client, err := h.Tracker()
	if err != nil {
		log.Event("loop … " + key + " committed-fix recovery: no tracker for the PR body: " + err.Error())
		return Recovery{Attempted: true}
	}
	t, err := client.FetchTicket(key)
	if err != nil {
		log.Event("loop … " + key + " committed-fix recovery: could not fetch ticket for the PR body: " + err.Error())
		return Recovery{Attempted: true}
	}

	out := Finish(h, log, slug, t)
	if !out.Pushed {
		log.Event("loop … " + key + " committed-fix recovery: push failed: " + out.Err.Error())
		return Recovery{Attempted: true}
	}
	if out.Err != nil {
		log.Event("loop … " + key + " committed-fix recovery: gh pr create failed: " + out.Err.Error())
		return Recovery{Attempted: true}
	}
	return Recovery{Attempted: true, Shipped: true}
}
