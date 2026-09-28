package ship

import "strings"

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
// On a recoverable branch it lands it through [Land] with no conflict hook: a
// content conflict needs the review stage's sandboxed session, not the daemon. Any
// verdict but Landed returns Attempted=true with Shipped=false, so the daemon falls
// through to its normal release and a later run re-grabs it — an attempt is never
// worse than no attempt. Opening a PR is safe recovery, not a merge: CI and human
// review still gate that.
func Recover(h Port, log EventSink, key string) Recovery {
	slug := strings.ToLower(key)

	// A committed fix means a worktree that is there and clean: uncommitted edits are
	// an in-progress or crashed run, not a finished-but-unpushed one, so those are
	// left to the normal release path.
	if !h.WorktreeExists(slug) || !h.WorktreeClean(slug) {
		return Recovery{}
	}
	// Refresh main so the empty-diff check is current.
	if err := h.FetchMain(); err != nil {
		log.Event("loop … warning: could not fetch origin/main during committed-fix recovery: " + err.Error())
	}
	// Nothing committed ahead of main (already merged, or an empty branch) → not a
	// recoverable committed fix.
	if h.BranchDiffEmpty(slug) {
		return Recovery{}
	}
	// A branch that already has an OPEN PR isn't stranded (its run would already be
	// Shipped — belt-and-braces), so there is nothing to finish.
	if h.OpenPRExists(slug) {
		return Recovery{}
	}

	log.Event("loop — " + key + " has a committed, unpushed fix with no PR; completing it host-side (rebase + push + PR) (BEH-713)")

	// The ticket is fetched BEFORE the landing, not after: the PR body needs it, and a
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

	landing := Land(h, log, slug, t, nil)
	switch landing.Verdict {
	case Landed:
		return Recovery{Attempted: true, Shipped: true}
	case Disjoint:
		log.Event("loop … " + key + " committed-fix recovery: branch has a disjoint history from main (BEH-597) — cannot auto-complete")
	case Conflict:
		log.Event("loop … " + key + " committed-fix recovery: rebase onto main conflicted — cannot auto-complete")
	case Collapsed:
		log.Event("loop … " + key + " committed-fix recovery: branch became empty after rebase — nothing to ship")
	case PushFailed:
		log.Event("loop … " + key + " committed-fix recovery: push failed: " + landing.Err.Error())
	case PRFailed:
		log.Event("loop … " + key + " committed-fix recovery: gh pr create failed: " + landing.Err.Error())
	}
	return Recovery{Attempted: true}
}
