package prloop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/fix"
	"github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// who is the name the PR loop's narration speaks in.
const who = "pr-fix"

// LogsRoot is where a PR loop keeps its per-PR log dirs and its own loop.jsonl
// (config.Loop.LogsDir), beside (not inside) the ticket loop's, so the daemons can
// run side by side and the viewer can follow either.
func LogsRoot(cfg config.Config, l config.Loop) string {
	return filepath.Join(cfg.ProjectPath, l.LogsDir())
}

// Pipeline is a PR loop's [loop.Pipeline]: [Fix] over one PR with the loop's
// Steps, on a Host bound to the PR's head branch, with its own per-PR log dir and
// run id.
type Pipeline struct {
	Loop config.Loop
	// Source is the loop's Source, which takes back the attempt it recorded when
	// git refuses the checkout on local state (#247) or the sandbox preflight
	// refuses (#232), and hands the PR to a human when a run's leftover work could
	// not be saved (#246). Nil leaves the attempt spent and the PR unlabelled.
	Source *Source
}

var _ loop.Pipeline = Pipeline{}

// Run fixes one held PR. It always gives the lease back when it is done — unlike a
// ticket, a PR is never kept: the label is the lease for this run only, and the
// next poll judges the PR afresh.
func (p Pipeline) Run(item loop.WorkItem, claim *lease.Lease) stages.Result {
	defer func() { _ = claim.Release() }()
	it, ok := item.Context.(Item)
	if !ok {
		fmt.Fprintf(os.Stderr, "prloop: %s carries no PR context\n", item.Key)
		return stages.Result{}
	}
	cfg, err := stages.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}
	}
	log, err := runlog.New(LogsRoot(cfg, p.Loop), item.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}
	}
	h := hostio.NewOnBranch(cfg, log, runlog.MakeRunID(time.Now()), false, ticket.Slug(item.Key), item.Branch)
	out := Fix(h, cfg, log, it, p.Loop)
	// Before the deferred release, so settling the lease sees the PR handed off.
	if p.Source != nil {
		var unsaved *UnsavedError
		switch {
		case out.Disposition == stages.CheckoutBlocked:
			p.Source.Withdraw(it, out.Err)
		case out.Disposition == stages.PreflightAborted:
			p.Source.TakeBack(it, out.Err)
		case errors.As(out.Err, &unsaved):
			p.Source.HandOver(it, unsaved)
		}
	}
	return out
}

// Recover finds nothing to recover: a PR run either pushed or left the branch as
// it found it, and the next poll judges the PR afresh.
func (Pipeline) Recover(loop.WorkItem) (stages.Result, bool) { return stages.Result{}, false }

// Fix is the PR-fixer pipeline over one PR, through h, running the loop's Steps
// (ADR-0019):
//
//  1. checkout_pr: check the PR's head branch out at the polled head, or at the
//     recovery checkpoint an earlier attempt left on it (#246), and run
//     post_create;
//  2. fix, gates, push_existing: a conflict runs the RebaseFix session (rebase on a
//     harness branch, merge on a human's), checked by verify.RebaseResolution and
//     then every gate, and pushes to the existing branch — force-with-lease after a
//     rebase, plain after a merge. A resolution the gates hold back is saved, and
//     the next run at the same head, on the same origin/main, re-gates and pushes
//     it with no session (#252);
//  3. watch_ci: watch CI to a verdict, and a red check gets a CIFix session over
//     the failing logs whose committed fix must pass every gate before the watch
//     may push it. A loop without it stops at the push, and works no red CI
//     (config validation sees to that);
//  4. retro: the retrospective mines the run's transcripts and files findings.
//
// The loop's fix and retro prompt bodies replace the defaults.
//
// Nothing is pushed unless the ground truth and the gates agree, and no PR is
// opened: the PR exists. Uncommitted work a session leaves is saved as a local
// recovery checkpoint when the run ends; if it cannot be, the result carries an
// [UnsavedError]. A push counts as Shipped for the breaker, as a ticket's
// does, even when CI is still red after the fix budget.
func Fix(h hostio.Host, cfg config.Config, log *runlog.Logger, it Item, l config.Loop) (out stages.Result) {
	l.ApplyPrompts(&cfg.Prompts)
	key := Key(it.PR.Number)
	slug := ticket.Slug(key)
	fixes := fix.Env{Host: h, Cfg: cfg, Log: log, Who: who}
	target := fix.Target{Slug: slug, Ticket: prTicket(key, it.PR), Branch: it.PR.HeadBranch}
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: key, Stage: who, Message: fmt.Sprintf("run %s — %s %s on %s at %s (%s)", h.RunID(), it.Kind.describe(), key, it.PR.HeadBranch, short(it.PR.HeadSHA), it.Strategy())})

	if err := h.Preflight(); err != nil {
		return stages.Result{Err: err, Disposition: stages.PreflightAborted}
	}
	if err := h.FetchMain(); err != nil {
		log.Event(who + " … warning: could not fetch origin/main: " + err.Error())
	}
	wt := h.Worktree()
	resumed, err := wt.CheckoutRemote()
	if err != nil {
		log.Event(fmt.Sprintf("%s ✗ could not check out %s: %v — nothing pushed", who, it.PR.HeadBranch, err))
		// git refused on the host's local state, before any session: not the PR's
		// failure, and no attempt (#247). A fetch that failed after its retries is.
		var local *git.LocalStateError
		if errors.As(err, &local) {
			return stages.Result{Err: err, Disposition: stages.CheckoutBlocked}
		}
		return stages.Result{}
	}
	r := run{h: h, cfg: cfg, log: log, fixes: fixes, target: target, it: it, watchCI: l.Has(config.StepWatchCI)}
	// The branch on origin is the record. What a session left uncommitted is saved
	// as a local recovery checkpoint, which the next attempt at this head starts
	// from (#246). If it cannot be saved, the next checkout would refuse the dirty
	// worktree, so the PR goes to a human now rather than spend its attempts.
	defer func() {
		if err := fixes.Checkpoint(key); err != nil {
			log.Event(fmt.Sprintf("%s ✗ could not save the uncommitted work in %s as a recovery checkpoint: %v — handing %s to a human", who, wt.Path(), err, key))
			out.OK, out.Err = false, &UnsavedError{Path: wt.Path(), Err: err}
		}
		fixes.Cleanup()
	}()

	// Work the head that was polled or nothing: a push that landed since is a newer
	// head, which the next poll judges as a new item. A resumed checkpoint sits on
	// origin's tip, which is the head.
	head, err := wt.HeadSHA()
	if resumed.Checkpoint != "" {
		head = resumed.Base
	}
	if err != nil || head != it.PR.HeadSHA {
		log.Event(fmt.Sprintf("%s ⊘ %s moved since the poll (now %s, polled %s) — leaving it to the next poll", who, it.PR.HeadBranch, short(head), short(it.PR.HeadSHA)))
		return stages.Result{}
	}
	if resumed.Checkpoint != "" {
		log.Event(fmt.Sprintf("%s ↺ resuming from the recovery checkpoint %s an earlier attempt left on %s — unverified, and the session is told so (#246)", who, short(resumed.Checkpoint), short(resumed.Base)))
		r.target.Checkpoint = &prompt.Checkpoint{Commit: resumed.Checkpoint, Base: resumed.Base}
	}
	if !fixes.Provision() {
		return stages.Result{}
	}

	out = r.repair()
	if r.sessions && l.Has(config.StepRetro) {
		t := target.Ticket
		t.TeamID = stages.FindingsTeam(h, log, who, key)
		stages.RetrospectiveOver(h, cfg, log, stages.Subject{
			Ticket: t, Slug: slug, WorktreePath: wt.Path(), Branch: wt.Branch(), LogDir: log.Dir,
		}, false)
	}
	return out
}

// UnsavedError is a run that ended with uncommitted work in its worktree at Path
// that the harness could not save as a recovery checkpoint (#246). The next
// checkout would refuse that worktree, so the PR goes to a human at once.
type UnsavedError struct {
	Path string
	Err  error
}

func (e *UnsavedError) Error() string {
	return fmt.Sprintf("worktree %s holds uncommitted work that could not be saved as a recovery checkpoint: %v", e.Path, e.Err)
}

func (e *UnsavedError) Unwrap() error { return e.Err }

// run is one PR fix in flight: what it acts through, and what it has done so far.
type run struct {
	h      hostio.Host
	cfg    config.Config
	log    *runlog.Logger
	fixes  fix.Env
	target fix.Target
	it     Item
	// watchCI is the loop's watch_ci step.
	watchCI bool

	// sessions marks a run that launched an agent session, which leaves the
	// transcripts a retrospective mines.
	sessions bool
	// pushed marks a run that changed the branch on origin.
	pushed bool
}

// repair fixes what is wrong with the PR and watches CI to a verdict.
func (r *run) repair() stages.Result {
	if r.it.Kind == Conflict {
		if out, ok := r.resolve(); !ok {
			return out
		}
		push := r.h.PushForceWithLease
		if r.it.Strategy() == fix.Merge {
			push = r.h.Push
		}
		if err := push(); err != nil {
			r.log.Event(fmt.Sprintf("%s ✗ push of %s failed: %v", who, r.it.PR.HeadBranch, err))
			return stages.Result{}
		}
		r.pushed = true
		r.h.Worktree().DropResolutions()
		r.log.Event(fmt.Sprintf("%s ✓ pushed the resolved %s (%s)", who, r.it.PR.HeadBranch, r.it.Strategy()))
	}
	if !r.watchCI {
		if r.pushed {
			return stages.Result{OK: true, Disposition: stages.Shipped}
		}
		return stages.Result{}
	}
	return r.watch()
}

// resolve brings the conflicting branch onto origin/main and reports whether it is
// clear to push; when it is not, out is the run's result. A resolution an earlier
// run at this head committed, whose push its red re-gate held back, is re-gated
// and pushed as it stands, with no session, while origin/main is still its base
// (#252). Otherwise a RebaseFix session resolves the conflict, and a resolution
// the ground truth accepts but the gates hold back is saved for the next run: the
// worktree is removed and the next checkout resets the branch to origin's tip.
func (r *run) resolve() (out stages.Result, ok bool) {
	wt := r.h.Worktree()
	head := r.it.PR.HeadSHA
	if tip, saved := wt.SavedResolution(head); saved && r.target.Checkpoint == nil {
		r.log.Event(fmt.Sprintf("%s ↺ resuming the conflict resolution %s an earlier run committed over %s on %s — re-gating it instead of launching a conflict-resolution session (#252)", who, short(tip), short(head), git.BaseRef))
		if err := wt.ResumeResolution(tip); err != nil {
			r.log.Event(fmt.Sprintf("%s ✗ could not move %s to the saved resolution %s: %v — nothing pushed", who, r.it.PR.HeadBranch, short(tip), err))
			return stages.Result{}, false
		}
		if gate := r.fixes.Gates("gate"); !gate.Green() || !wt.Clean() {
			r.log.Event(fmt.Sprintf("%s ✗ gate re-run over the saved resolution failed (exit %d) — nothing pushed; the resolution stays saved for the next run at this head (#252)", who, gate.Outcome.ExitCode))
			return stages.Result{}, false
		}
		r.log.Event(who + " ✓ gate re-run over the saved resolution is green — clear to push")
		return stages.Result{}, true
	}
	r.sessions = true
	res := r.fixes.ResolveConflict(fix.Conflict{
		Target:   r.target,
		Strategy: r.it.Strategy(),
		Regate:   func() fix.GateResult { return r.fixes.Gates("gate") },
	})
	switch {
	case res.OK:
		return stages.Result{}, true
	case res.SpendingCapAbort:
		return stages.Result{Disposition: stages.CapAborted}, false
	case res.Resolved:
		if tip, err := wt.SaveResolution(head); err != nil {
			r.log.Event(fmt.Sprintf("%s … warning: could not save the committed resolution of %s: %v — the next run resolves it again", who, short(head), err))
		} else {
			r.log.Event(fmt.Sprintf("%s ⚑ saved the committed resolution %s of %s as a local ref, not pushed — the next run at this head re-gates it instead of resolving again (#252)", who, short(tip), short(head)))
		}
	}
	return stages.Result{}, false
}

// watch watches CI to a terminal verdict. A red check gets the gated CI fix: a
// CIFix session over the failing logs, whose committed fix must pass every gate
// before the watch may push it. A branch a human opted in is never reactively
// rebased.
func (r *run) watch() stages.Result {
	gated := r.fixes.GatedCI(r.target)
	r.log.Event("watching CI for " + r.it.PR.HeadBranch + " …")
	res := r.h.WatchCI(hostio.CIWatch{Fix: gated.Fix, KeepHistory: !r.it.Harness})
	r.sessions = r.sessions || gated.Attempts > 0
	r.pushed = r.pushed || gated.Accepted > 0
	switch {
	case res.OK:
		r.log.Event(who + " ✓ " + res.Reason)
		return stages.Result{OK: true, Disposition: stages.Shipped}
	case res.SpendingCapAbort && !r.pushed:
		r.log.Event(who + " ↻ CI auto-fix deferred — spending cap reached, retry after reset")
		return stages.Result{Disposition: stages.CapAborted}
	}
	r.log.Event(who + " ✗ CI did not go green: " + res.Reason)
	if r.pushed {
		return stages.Result{Disposition: stages.Shipped}
	}
	return stages.Result{}
}

// prTicket is the PR as the sessions' prompts see it: the intent is the PR's
// title and description.
func prTicket(key string, pr github.PullRequest) ticket.Ticket {
	return ticket.Ticket{Identifier: key, Title: pr.Title, Description: pr.Body, URL: pr.URL}
}
