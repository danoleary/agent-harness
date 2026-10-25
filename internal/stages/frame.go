package stages

import (
	"fmt"
	"os"

	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// frame is the lifecycle every Stage shares (#29): it announces the Stage,
// fetches the ticket, readies the findings dropbox and narrates each sandbox
// session, so the Stage bodies hold only what they decide. It is a set of steps
// rather than a wrapper around the body, because the bodies interleave their own
// policy between them — the retrospective's skip comes before the fetch, the
// implementation's already-on-main skip between the dropbox and the preflight —
// and a wrapper would have to reorder one of them to own the rest.
//
// The preflight and the findings filing stay in the bodies: each Stage answers a
// failed preflight differently (only implementation releases the claim and
// reports PreflightAborted), and the implementation stage files its dropbox
// audience-blind while the retrospective routes it by audience (ADR-0011).
type frame struct {
	h   hostio.Host
	log *runlog.Logger
	// stage is the Stage's session name: its loop.jsonl Stage, and the prefix of
	// its transcripts and findings dir.
	stage string
	key   string
}

func newFrame(h hostio.Host, log *runlog.Logger, stage, key string) frame {
	return frame{h: h, log: log, stage: stage, key: key}
}

// start tells the viewer the Stage began, naming the run and whether it is only
// a plan.
func (f frame) start(dryRun bool) {
	dry := ""
	if dryRun {
		dry = " (dry-run)"
	}
	f.log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: f.key, Stage: f.stage, Message: fmt.Sprintf("run %s — %s %s%s", f.h.RunID(), f.stage, f.key, dry)})
}

// fetch resolves the Consumer's tracker and fetches the ticket, the intent every
// Stage works against. Neither claims nor moves it: the lease owns that (#25).
func (f frame) fetch() (tracker.Tracker, ticket.Ticket, error) {
	client, err := f.h.Tracker()
	if err != nil {
		return nil, ticket.Ticket{}, err
	}
	t, err := client.FetchTicket(f.key)
	if err != nil {
		return nil, ticket.Ticket{}, err
	}
	priority := t.Priority
	if priority == "" {
		priority = "No priority"
	}
	f.log.Event(fmt.Sprintf("fetched %s (%s) — %s", t.Identifier, priority, t.Title))
	return client, t, nil
}

// dropbox creates the session's findings dir and clears a stale out.json a prior
// run of this ticket left: the dir is ticket+session keyed and reused across runs,
// so a leftover would both re-file as duplicates and satisfy the "out.json
// present" ground truth for a session that never wrote. ok is false when the
// Stage must stop with res.
//
// A full host disk fails the mkdir with ENOSPC. That is not the Stage's fault and
// the in-sandbox agent can't fix it, so it degrades to a clear, actionable
// warning and a failed Stage rather than an opaque hard error that masquerades as
// a crash and forces a manual re-run (BEH-540).
func (f frame) dropbox(dir string) (res Result, ok bool) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if isDiskFull(err) {
			f.log.Event(diskFullWarning(f.stage, err))
			return Result{OK: false}, false
		}
		return Result{Err: err}, false
	}
	if err := filing.ClearDropbox(dir); err != nil {
		return Result{Err: err}, false
	}
	return Result{}, true
}

// launch runs one sandboxed session: it tells the viewer the sandbox is
// launching, with the run's active-time cap and, for a Stage that launches more
// than once, which launch this is (note), then says where the transcript went.
func (f frame) launch(run hostio.AgentRun, note string) hostio.Result {
	if note != "" {
		note += ", "
	}
	f.log.Structured(loopstream.Record{Kind: loopstream.KindSandboxLaunch, Ticket: f.key, Stage: f.stage, Message: fmt.Sprintf("launching sandbox (%scap %d min active)", note, int(run.Cap.Minutes()))})
	res := f.h.Agent(run)
	f.log.Event(fmt.Sprintf("%s session exited (code %d) — transcript at logs/%s/%s", f.stage, res.ExitCode, f.key, res.Transcript))
	return res
}

// interrupted reports whether a stop signal killed the session (#99) and, when it
// did, narrates it and returns the Stage's Interrupted Result. The Stage returns
// that at once: a session cut short handed nothing off, so its ground-truth check
// would only record a failure that never happened, and whatever sent the signal
// is likely taking this process down too. The worktree and the claim are left as
// they are for the next run.
func (f frame) interrupted(o session.Outcome) (Result, bool) {
	res, ok := InterruptedResult(o)
	if ok {
		f.log.Event(fmt.Sprintf("%s ⏸ session interrupted by signal %d (%s) — not a failure; skipping verification and leaving the worktree and claim for the next run (#99)", f.stage, int(res.Signal), res.Signal))
	}
	return res, ok
}
