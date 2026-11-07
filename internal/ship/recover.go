package ship

import (
	"fmt"
	"strings"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
)

// Recovery is what the committed-fix recovery learned about one ticket.
// Attempted is the "was there anything to finish?" signal the daemon branches on:
// false means this was a genuine no-PR run (an OOM, a crash, an empty diff, a tip no
// host gate covers) that should be released to Todo exactly as before, so a recovery
// that finds nothing is never mistaken for one that failed.
type Recovery struct {
	// Attempted is true only when the branch really was in the committed-but-unshipped
	// state and the recovery ran.
	Attempted bool
	// Shipped is true when the branch is now pushed with an open PR.
	Shipped bool
	// Refused says why the recovery would not land a branch the review stage did not
	// clear — a blocked verdict or a red Gate over the tip (#230). Non-empty only with
	// Attempted=true, Shipped=false: the daemon hands the ticket to a human with it
	// rather than releasing it to Todo, where it would be re-run to the same refusal.
	Refused string
}

// PushGate is the review stage's last push decision over a branch, as it recorded it
// in the ticket's log dir: the commit its host Gate re-run covered, how that re-run
// exited, and the verdict the review session emitted over it (#230). It is what
// tells the recovery a gate-green, reviewed commit from one the review refused.
type PushGate struct {
	// Head is the commit the review and its host Gate re-run covered.
	Head string
	// Disposition is the verdict line the review emitted ("Disposition: clear — …"),
	// empty when it emitted none (the BEH-713 incompleteness).
	Disposition string
	// Blocked is whether that verdict declared a blocked Disposition (BEH-580).
	Blocked bool
	// GateRan is whether a host Gate re-run is recorded at all. A record written
	// before the push gate was recorded carries only the verdict.
	GateRan bool
	// GateExit is the host Gate re-run's exit code, and FailedGate the name of the
	// Gate that went red ("" when green, or when the failure named no Gate).
	GateExit   int
	FailedGate string
}

// refusal says why gate does not clear its Head for landing, or "" when it does.
func (g PushGate) refusal() string {
	var why []string
	if g.Blocked {
		d := g.Disposition
		if d == "" {
			d = "Disposition: blocked"
		}
		why = append(why, "the review declared a blocked Disposition ("+d+")")
	}
	if g.GateExit != 0 {
		gate := fmt.Sprintf("the host gate re-run was red (exit %d)", g.GateExit)
		if g.FailedGate != "" {
			gate = fmt.Sprintf("the host gate %q was red (exit %d)", g.FailedGate, g.GateExit)
		}
		why = append(why, gate)
	}
	return strings.Join(why, " and ")
}

// Recover finishes a no-PR run whose fix was already committed on its branch but
// never pushed or PR'd — the recovered-checkpoint / verify-only state where the
// review stage left a gate-green, clean commit yet never reached a PR (BEH-713,
// the BEH-649 infinite Todo↔In-Progress bounce). Releasing such a ticket just
// re-grabs it to the same no-verdict conclusion forever, so before the daemon
// treats a no-PR run as a failure it tries to finish it here. h is bound to key's
// ticket; key itself is for the PR body and the narration. gate is the review
// stage's last recorded [PushGate] for the ticket, nil when there is none.
//
// It reports Attempted=true ONLY when there was such a state to finish: a worktree
// on disk, clean, carrying commits against origin/main, with no open PR, whose tip
// gate covers with a recorded host Gate re-run. Every other shape — a tip no Gate
// re-run covers included — returns the zero Recovery and the daemon releases as
// before, so a later run reviews it.
//
// A covered tip the review did not clear — gate records a blocked verdict or a red
// Gate over it — is refused, not landed (#230): Attempted=true, Shipped=false, and
// Refused says why, for the daemon to hand the ticket to a human with. The worktree
// is kept and nothing reaches the remote.
//
// A cleared tip — green Gates, no blocked verdict; an empty verdict is the BEH-713
// case this exists for — is landed through [Land] with no conflict hook: a content
// conflict needs the review stage's sandboxed session, not the daemon. Any verdict
// but Landed returns Attempted=true with Shipped=false, so the daemon falls through
// to its normal release and a later run re-grabs it — an attempt is never worse
// than no attempt. A Landed branch has its worktree reaped (see reap). Opening a PR
// is safe recovery, not a merge: CI and human review still gate that.
func Recover(h Port, log EventSink, key string, gate *PushGate) Recovery {
	wt := h.Worktree()

	// A committed fix means a worktree that is there and clean: uncommitted edits are
	// an in-progress or crashed run, not a finished-but-unpushed one, so those are
	// left to the normal release path.
	if !wt.Exists() || !wt.Clean() {
		return Recovery{}
	}
	// Refresh main so the empty-diff check is current.
	if err := h.FetchMain(); err != nil {
		log.Event("loop … warning: could not fetch origin/main during committed-fix recovery: " + err.Error())
	}
	// Nothing committed ahead of main (already merged, or an empty branch) → not a
	// recoverable committed fix.
	if wt.DiffEmpty() {
		return Recovery{}
	}
	// A branch that already has an OPEN PR isn't stranded (its run would already be
	// Shipped — belt-and-braces), so there is nothing to finish.
	if h.OpenPRExists() {
		return Recovery{}
	}

	// Only a tip the review's host Gate re-run covered is the gate-green commit this
	// exists to finish; anything committed since — or a run that never recorded its
	// gate — is unverified, so a later run must review it.
	head, _ := wt.HeadSHA()
	if gate == nil || !gate.GateRan || head == "" || gate.Head != head {
		log.Event("loop … " + key + " has a committed, unpushed tip " + head + " but no host gate covers it — not completing it; a later run reviews it (#230)")
		return Recovery{}
	}
	if why := gate.refusal(); why != "" {
		log.Event("loop ✗ " + key + " committed-fix recovery: refusing to land " + head + " — " + why + "; handing it to a human (#230)")
		return Recovery{Attempted: true, Refused: why}
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

	landing := Land(h, log, t, nil)
	switch landing.Verdict {
	case Landed:
		reap(wt, log, key)
		return Recovery{Attempted: true, Shipped: true}
	case Disjoint:
		log.Event("loop … " + key + " committed-fix recovery: branch has a disjoint history from main (BEH-597) — cannot auto-complete")
	case Conflict:
		log.Event("loop … " + key + " committed-fix recovery: rebase onto main conflicted — cannot auto-complete")
	case RebaseFailed:
		log.Event("loop … " + key + " committed-fix recovery: rebase onto main failed with no content conflict (#164) — cannot auto-complete")
	case Collapsed:
		log.Event("loop … " + key + " committed-fix recovery: branch became empty after rebase — nothing to ship")
	case PushFailed:
		log.Event("loop … " + key + " committed-fix recovery: push failed: " + landing.Err.Error())
	case PRFailed:
		log.Event("loop … " + key + " committed-fix recovery: gh pr create failed: " + landing.Err.Error())
	}
	return Recovery{Attempted: true}
}

// reap tears down a recovered ticket's worktree once its PR is open (#184). The
// slice's retrospective already ran and kept the worktree only because the branch
// had no PR — the committed-fix recovery needed it (verify.WorktreeReap) — and no
// retrospective follows a recovery. Left checked out, the ticket loop would hold the
// branch for as long as the ticket stays In Progress, so the PR loop could never fix
// the PR. A failed removal is narrated, not fatal: the PR is open either way.
func reap(wt gitpkg.Worktree, log EventSink, key string) {
	if err := wt.Remove(); err != nil {
		log.Event("loop … " + key + " committed-fix recovery: worktree kept — removal failed: " + err.Error())
		return
	}
	log.Event("loop — " + key + " committed-fix recovery: worktree removed — PR open, branch free for the PR loop (#184)")
}
