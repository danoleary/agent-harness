package prloop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/findings"
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
		case out.Disposition == stages.BaseRed:
			p.Source.HoldOnBase(it, out.HeldBase, out.HeldGate)
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
//     it with no session (#252). A gate whose skip_when_only matches every path of
//     the diff against origin/main is skipped, here and over a CI fix (#221). A pushed resolution that renumbered one of the
//     branch's ADRs has the PR's title and description rewritten to the new
//     number (#267);
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
	fixes := fix.Env{Host: h, Cfg: cfg, Log: log, Who: who, SkipWhenOnly: true}
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
	// overlaps is what the replay's session reported the resolved branch
	// duplicates of what origin/main added under it (#255), for the PR (#344).
	overlaps []fix.Overlap
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
		r.renumberDescription()
		r.reportOverlaps()
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
		r.overlaps = res.Overlaps
		return stages.Result{}, true
	case res.SpendingCapAbort:
		return stages.Result{Disposition: stages.CapAborted}, false
	case res.Resolved:
		if tip, err := wt.SaveResolution(head); err != nil {
			r.log.Event(fmt.Sprintf("%s … warning: could not save the committed resolution of %s: %v — the next run resolves it again", who, short(head), err))
		} else {
			r.log.Event(fmt.Sprintf("%s ⚑ saved the committed resolution %s of %s as a local ref, not pushed — the next run at this head re-gates it instead of resolving again (#252)", who, short(tip), short(head)))
		}
		if res.BaseRed != nil {
			return r.holdOnBase(*res.BaseRed), false
		}
	}
	return stages.Result{}, false
}

// renumberDescription rewrites the PR's title and description after a pushed
// resolution renumbered one of the branch's ADRs because main had merged one
// under the same number (ADR-0034). The session cannot edit the PR, so without
// this the description goes on citing a number that now names main's decision
// (#267). The branch is already pushed, so a failure is only narrated.
func (r *run) renumberDescription() {
	moved, err := r.h.Worktree().RenumberedADRs(r.it.PR.HeadSHA)
	if err != nil {
		r.log.Event(fmt.Sprintf("%s … warning: could not tell whether the resolution renumbered an ADR: %v — the PR description is unchanged", who, err))
		return
	}
	if len(moved) == 0 {
		return
	}
	title, body := renumberADRs(r.it.PR.Title, moved), renumberADRs(r.it.PR.Body, moved)
	if title == r.it.PR.Title && body == r.it.PR.Body {
		return
	}
	var names []string
	for _, m := range moved {
		names = append(names, "ADR-"+m.From+" → ADR-"+m.To)
	}
	if err := r.h.EditPR(r.it.PR.Number, title, body); err != nil {
		r.log.Event(fmt.Sprintf("%s ⚠ the resolution renumbered %s, but the PR description could not be updated: %v — it still cites the old number (#267)", who, strings.Join(names, ", "), err))
		return
	}
	r.it.PR.Title, r.it.PR.Body = title, body
	r.log.Event(fmt.Sprintf("%s ✓ updated the PR title and description for the renumbered %s (#267)", who, strings.Join(names, ", ")))
}

// reportOverlaps puts the overlap report the pushed resolution's session wrote
// on the open PR, as its description's `## Overlap with main` section, in place
// of one an earlier replay left (#344). The review stage's own section is out of
// reach here, since the PR is already open (ADR-0050), and the report is a
// design question for whoever merges it, not for run.jsonl alone. The branch is
// already pushed, so a failure is only narrated.
func (r *run) reportOverlaps() {
	body := fix.UpsertOverlapSection(r.it.PR.Body, fix.OverlapSection(fix.OpenPROverlapIntro, r.overlaps))
	if body == r.it.PR.Body {
		return
	}
	if err := r.h.EditPR(r.it.PR.Number, r.it.PR.Title, body); err != nil {
		r.log.Event(fmt.Sprintf("%s ⚠ could not put the conflict resolution's overlap report on the PR: %v — it is in run.jsonl only (#344)", who, err))
		return
	}
	r.it.PR.Body = body
	r.log.Event(fmt.Sprintf("%s ⚑ put the conflict resolution's overlap report on the PR description (#344)", who))
}

// adrRef is an `ADR-NNNN` reference.
var adrRef = regexp.MustCompile(`\bADR-([0-9][0-9][0-9][0-9])\b`)

// renumberADRs rewrites text's references to each renumbered ADR, `ADR-<old>`
// and its file name, to the new number, all at once, so a chain of renumbers
// (0040 → 0041 while 0041 → 0042) moves each reference once.
func renumberADRs(text string, moved []git.Renumber) string {
	to := map[string]string{}
	var files []string
	for _, m := range moved {
		to[m.From] = m.To
		files = append(files, m.FromFile, m.ToFile)
	}
	text = strings.NewReplacer(files...).Replace(text)
	return adrRef.ReplaceAllStringFunc(text, func(ref string) string {
		if n, ok := to[adrRef.FindStringSubmatch(ref)[1]]; ok {
			return "ADR-" + n
		}
		return ref
	})
}

// holdOnBase ends a run whose resolution was red only because origin/main is
// (#250): a BaseRed run, held on that base until main moves or its red verdict
// expires (ADR-0039), which spends none of the PR's attempts. The run that
// recorded the red verdict files the base-gates-red finding, as the
// implementation stage does for a ticket (ADR-0036); one that read it leaves the
// filing to whoever recorded it.
func (r *run) holdOnBase(base fix.BaseRed) stages.Result {
	key := Key(r.it.PR.Number)
	r.log.Event(fmt.Sprintf("%s ⏸ holding %s on the red base %s until main moves — not counted as an attempt (#250)", who, key, short(base.SHA)))
	if base.Recorded {
		r.fileBaseRed(base)
	}
	return stages.Result{Disposition: stages.BaseRed, HeldBase: base.SHA, HeldUntil: base.Verdict.RecheckAt(), HeldGate: base.Verdict.Gate}
}

// fileBaseRed files the finding a red base raises (basegate.Finding) into the
// tracker's findings team.
func (r *run) fileBaseRed(base fix.BaseRed) {
	key := Key(r.it.PR.Number)
	body := fmt.Sprintf(
		"The harness resolved %s's conflict with `main` (`%s`), and its re-gate went red. Gated alone, with nothing of the branch on it, `main` at `%s` fails gate `%s` (exit %d) too, so the red is main's.\n\n"+
			"The PR loop holds until `main` moves rather than spend the PR's attempts on a red it cannot fix (#250), and the ticket loop holds every ticket cut from this base (#125).\n\n"+
			"Repair `main` directly, or dispatch this issue by hand (`agent-harness pipeline <this issue>`): an issue carrying this finding key is exempt from the hold.",
		key, r.it.PR.HeadBranch, base.SHA, base.Verdict.Gate, base.Gate.Outcome.ExitCode,
	)
	f := basegate.Finding(base.Verdict.Gate, base.Gate.Outcome.ExitCode, base.Gate.Outcome.StderrTail, body)
	dir := r.log.FindingsDir("basegate")
	err := os.MkdirAll(dir, 0o755)
	if err == nil {
		err = filing.WriteDropbox(dir, []findings.Finding{f})
	}
	if err != nil {
		r.log.Event("⚠ could not write the red-base finding (" + err.Error() + ")")
		return
	}
	r.h.FileFindings(dir, stages.FindingsTeam(r.h, r.log, who, key), key)
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
