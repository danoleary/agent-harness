package prloop

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/fix"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/verify"
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
	return Fix(h, cfg, log, it, p.Loop)
}

// Recover finds nothing to recover: a PR run either pushed or left the branch as
// it found it, and the next poll judges the PR afresh.
func (Pipeline) Recover(loop.WorkItem) (stages.Result, bool) { return stages.Result{}, false }

// Fix is the PR-fixer pipeline over one PR, through h, running the loop's Steps
// (ADR-0019):
//
//  1. checkout_pr: check the PR's head branch out at the polled head, and run
//     post_create;
//  2. fix, gates, push_existing: a conflict runs the RebaseFix session (rebase on a
//     harness branch, merge on a human's), checked by verify.RebaseResolution and
//     then every gate, and pushes to the existing branch — force-with-lease after a
//     rebase, plain after a merge;
//  3. watch_ci: watch CI to a verdict, and a red check gets a CIFix session over
//     the failing logs whose committed fix must pass every gate before the watch
//     may push it. A loop without it stops at the push, and works no red CI
//     (config validation sees to that);
//  4. retro: the retrospective mines the run's transcripts and files findings.
//
// The loop's fix and retro prompt bodies replace the defaults.
//
// Nothing is pushed unless the ground truth and the gates agree, and no PR is
// opened: the PR exists. A push counts as Shipped for the breaker, as a ticket's
// does, even when CI is still red after the fix budget.
func Fix(h hostio.Host, cfg config.Config, log *runlog.Logger, it Item, l config.Loop) stages.Result {
	l.ApplyPrompts(&cfg.Prompts)
	key := Key(it.PR.Number)
	slug := ticket.Slug(key)
	fixes := fix.Env{Host: h, Cfg: cfg, Log: log, Who: who}
	target := fix.Target{Slug: slug, Ticket: prTicket(cfg, key, it.PR), Branch: it.PR.HeadBranch}
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: key, Stage: who, Message: fmt.Sprintf("run %s — %s %s on %s at %s (%s)", h.RunID(), it.Kind.describe(), key, it.PR.HeadBranch, short(it.PR.HeadSHA), it.Strategy())})

	if err := h.Preflight(); err != nil {
		return stages.Result{Err: err, Disposition: stages.PreflightAborted}
	}
	if err := h.FetchMain(); err != nil {
		log.Event(who + " … warning: could not fetch origin/main: " + err.Error())
	}
	if err := h.CheckoutBranch(slug); err != nil {
		log.Event(fmt.Sprintf("%s ✗ could not check out %s: %v — nothing pushed", who, it.PR.HeadBranch, err))
		return stages.Result{}
	}
	r := run{h: h, cfg: cfg, log: log, fixes: fixes, target: target, it: it, watchCI: l.Has(config.StepWatchCI)}
	defer r.cleanup()

	// Work the head that was polled or nothing: a push that landed since is a newer
	// head, which the next poll judges as a new item.
	if head, err := h.HeadSHA(slug); err != nil || head != it.PR.HeadSHA {
		log.Event(fmt.Sprintf("%s ⊘ %s moved since the poll (now %s, polled %s) — leaving it to the next poll", who, it.PR.HeadBranch, short(head), short(it.PR.HeadSHA)))
		return stages.Result{}
	}
	if !r.provision() {
		return stages.Result{}
	}

	out := r.repair()
	if r.sessions && l.Has(config.StepRetro) {
		stages.RetrospectiveOver(h, cfg, log, stages.Subject{
			Ticket: target.Ticket, Slug: slug, WorktreePath: h.WorktreePath(slug), Branch: h.BranchName(slug), LogDir: log.Dir,
		}, false)
	}
	return out
}

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

// provision runs the Consumer's post_create in the fresh worktree, so the sessions
// and the gates open onto installed dependencies. A failed install only warns (a
// session can install in-session), unless the shell could not run the command at
// all, which no session can fix (#46).
func (r *run) provision() bool {
	if r.cfg.PostCreate == "" {
		return true
	}
	res := r.h.Shell(hostio.ShellRun{Label: "prep", Command: r.cfg.PostCreate, WorktreePath: r.h.WorktreePath(r.target.Slug), Cap: r.cfg.ReviewTimeout})
	if missing, ok := verify.CommandUnrunnable(res.Outcome); ok {
		r.log.Event(fmt.Sprintf("%s ✗ post_create could not run (%q, exit %d) — fix the sandbox image's PATH; nothing pushed", who, missing, res.ExitCode))
		return false
	}
	if res.ExitCode != 0 {
		r.log.Event(fmt.Sprintf("%s … warning: post_create exited %d — sessions will provision in-session if needed", who, res.ExitCode))
	}
	return true
}

// repair fixes what is wrong with the PR and watches CI to a verdict.
func (r *run) repair() stages.Result {
	slug := r.target.Slug
	if r.it.Kind == Conflict {
		r.sessions = true
		res := r.fixes.ResolveConflict(fix.Conflict{
			Target:   r.target,
			Strategy: r.it.Strategy(),
			Regate:   func() fix.GateResult { return r.fixes.Gates(slug, "gate") },
		})
		if !res.OK {
			if res.SpendingCapAbort {
				return stages.Result{Disposition: stages.CapAborted}
			}
			return stages.Result{}
		}
		push := r.h.PushForceWithLease
		if r.it.Strategy() == fix.Merge {
			push = r.h.Push
		}
		if err := push(slug); err != nil {
			r.log.Event(fmt.Sprintf("%s ✗ push of %s failed: %v", who, r.it.PR.HeadBranch, err))
			return stages.Result{}
		}
		r.pushed = true
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

// watch watches CI to a terminal verdict. A red check gets a CIFix session over the
// failing logs, and its committed fix must pass every gate before the watch may
// push it. A branch a human opted in is never reactively rebased.
func (r *run) watch() stages.Result {
	slug := r.target.Slug
	ciFix := r.fixes.CI(r.target)
	attempt := 0
	gated := func(logs string, available bool) error {
		attempt++
		r.sessions = true
		if err := ciFix(logs, available); err != nil {
			return err
		}
		gate := r.fixes.Gates(slug, fmt.Sprintf("gate-cifix%d", attempt))
		if !gate.Green() {
			return fmt.Errorf("gate %q is red over auto-fix %d (exit %d) — not pushing", gate.FailedGate, attempt, gate.Outcome.ExitCode)
		}
		// The watch pushes an accepted fix next.
		r.pushed = true
		return nil
	}
	r.log.Event("watching CI for " + r.it.PR.HeadBranch + " …")
	res := r.h.WatchCI(hostio.CIWatch{Slug: slug, Fix: gated, KeepHistory: !r.it.Harness})
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

// cleanup removes the PR's worktree once it is clean: the branch on origin is the
// record, and the next run checks it out afresh. A worktree left dirty is kept for
// a look, and the next CheckoutBranch refuses to drop it.
func (r *run) cleanup() {
	slug := r.target.Slug
	if !r.h.WorktreeClean(slug) {
		r.log.Event(fmt.Sprintf("%s — worktree %s kept: it holds uncommitted changes", who, r.h.WorktreePath(slug)))
		return
	}
	if err := r.h.RemoveWorktree(slug); err != nil {
		r.log.Event(fmt.Sprintf("%s — worktree %s kept: %v", who, r.h.WorktreePath(slug), err))
	}
}

// prTicket is the PR as the sessions' prompts and the retrospective's filing see
// it: the intent is the PR's title and description. TeamID is where project
// findings file to; with GitHub Issues as the tracker that is the configured
// repository, and otherwise no ticket names a team for a PR, so only harness
// findings are routed.
func prTicket(cfg config.Config, key string, pr github.PullRequest) ticket.Ticket {
	t := ticket.Ticket{Identifier: key, Title: pr.Title, Description: pr.Body, URL: pr.URL}
	if cfg.Tracker.Kind == "github" {
		t.TeamID = cfg.Tracker.Repo
	}
	return t
}
