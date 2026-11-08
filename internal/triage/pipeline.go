package triage

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/stages"
)

// who is the name the triage loop's narration speaks in.
const who = "triage"

// Sandbox is the slice of the sandbox a triage run launches through.
type Sandbox interface {
	Preflight() error
	Agent(hostio.AgentRun) hostio.Result
}

// Pipeline is the triage loop's [loop.Pipeline]: the assess step, then the apply
// step when the loop has it.
type Pipeline struct {
	Loop    config.Loop
	Cfg     config.Config
	Backlog Backlog
	Store   Store
	// NewSandbox binds a sandbox to one run's log; nil is the real Docker runner.
	NewSandbox func(cfg config.Config, log *runlog.Logger) Sandbox
}

var _ loop.Pipeline = Pipeline{}

// Recover finds nothing to recover: a triage run commits nothing.
func (Pipeline) Recover(loop.WorkItem) (stages.Result, bool) { return stages.Result{}, false }

// Run triages one ticket.
//
//  1. assess: a read-only Triage session over the ticket, with the checkout
//     mounted so it can check the ticket against the code. Its ground truth is
//     the verdict file it must leave (ReadVerdict).
//  2. apply: a ready ticket is marked ready; one that is not gets a comment with
//     the questions. A loop without apply only records the verdict, which is how
//     to try triage out before letting it touch the tracker.
//
// The verdict is recorded only once it has been acted on, so a failed label is
// retried on the next poll. A recorded verdict counts as Shipped for the breaker:
// "not ready" is a verdict reached, not a failure.
func (p Pipeline) Run(item loop.WorkItem, _ *lease.Lease) stages.Result {
	it, ok := item.Context.(Item)
	if !ok {
		fmt.Fprintf(os.Stderr, "triage: %s carries no ticket context\n", item.Key)
		return stages.Result{}
	}
	log, err := runlog.New(filepath.Join(p.Cfg.ProjectPath, p.Loop.LogsDir()), item.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}
	}
	newSandbox := p.NewSandbox
	if newSandbox == nil {
		newSandbox = func(cfg config.Config, log *runlog.Logger) Sandbox {
			return hostio.NewRunner(cfg, log, runlog.MakeRunID(time.Now()), false)
		}
	}
	sb := newSandbox(p.Cfg, log)
	key := item.Key
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: key, Stage: who, Message: fmt.Sprintf("triaging %s: %s", key, it.Ticket.Title)})

	if err := sb.Preflight(); err != nil {
		log.Event(fmt.Sprintf("%s ✗ sandbox preflight failed: %v", who, err))
		return stages.Result{Err: err, Disposition: stages.PreflightAborted}
	}

	v, res, ok := p.assess(sb, log, it)
	if !ok {
		return res
	}

	rec := Record{Fingerprint: it.Fingerprint, Ready: v.Ready, Reason: v.Reason, Questions: v.Questions, At: time.Now().UTC()}
	if p.Loop.Has(config.StepApply) {
		if !p.apply(log, key, v) {
			return stages.Result{}
		}
		rec.Applied = true
	} else {
		log.Event(fmt.Sprintf("%s — no apply step: verdict recorded, %s left unchanged", who, key))
	}
	if err := p.Store.Save(key, rec); err != nil {
		log.Event(fmt.Sprintf("%s … warning: could not record the verdict on %s (%v); it will be judged again", who, key, err))
	}
	return stages.Result{OK: true, Disposition: stages.Shipped}
}

// assess runs the Triage session and reads its verdict back.
func (p Pipeline) assess(sb Sandbox, log *runlog.Logger, it Item) (Verdict, stages.Result, bool) {
	dropbox := log.FindingsDir(who)
	if err := os.MkdirAll(dropbox, 0o755); err != nil {
		log.Event(fmt.Sprintf("%s ✗ could not create the verdict dropbox: %v", who, err))
		return Verdict{}, stages.Result{}, false
	}
	verdictPath := filepath.Join(dropbox, prompt.TriageVerdictFile)
	_ = os.Remove(verdictPath)

	cfg := p.Cfg
	p.Loop.ApplyPrompts(&cfg.Prompts)
	pctx := prompt.Context{Ticket: it.Ticket, Tracker: cfg.Tracker.Kind, Body: cfg.Prompts.Triage}
	out := sb.Agent(hostio.AgentRun{
		Label:       who,
		Model:       cfg.ReviewModel,
		Prompt:      prompt.For(prompt.Triage, pctx),
		Warnings:    prompt.BodyWarnings(pctx),
		FindingsDir: dropbox,
		Cap:         cfg.ReviewTimeout,
	})
	if res, interrupted := stages.InterruptedResult(out.Outcome); interrupted {
		log.Event(fmt.Sprintf("%s — session interrupted; %s left as it was", who, it.Ticket.Identifier))
		return Verdict{}, res, false
	}
	if out.SpendingCapAbort {
		log.Event(fmt.Sprintf("%s ↻ spending cap reached; %s left as it was", who, it.Ticket.Identifier))
		return Verdict{}, stages.Result{Disposition: stages.CapAborted, CapResetAt: out.SpendingCapResetTime}, false
	}
	v, err := ReadVerdict(verdictPath)
	if err != nil {
		log.Event(fmt.Sprintf("%s ✗ no usable verdict on %s (exit %d, transcript %s): %v", who, it.Ticket.Identifier, out.ExitCode, out.Transcript, err))
		return Verdict{}, stages.Result{}, false
	}
	if v.Ready {
		log.Event(fmt.Sprintf("%s ✓ %s is ready for an agent: %s", who, it.Ticket.Identifier, v.Reason))
	} else {
		log.Event(fmt.Sprintf("%s ⊘ %s is not ready (%d question(s)): %s", who, it.Ticket.Identifier, len(v.Questions), v.Reason))
	}
	return v, stages.Result{}, true
}

// apply acts on the verdict in the tracker, and reports whether it did: the
// ready mark, or the not-ready comment. The note beside a ready mark is
// best-effort.
func (p Pipeline) apply(log *runlog.Logger, key string, v Verdict) bool {
	if v.Ready {
		if err := p.Backlog.MarkReady(key); err != nil {
			log.Event(fmt.Sprintf("%s ✗ could not mark %s ready: %v — it will be judged again", who, key, err))
			return false
		}
		log.Event(fmt.Sprintf("%s ✓ marked %s ready", who, key))
		if err := p.Backlog.AddComment(key, readyComment(v)); err != nil {
			log.Event(fmt.Sprintf("%s … warning: could not comment on %s: %v", who, key, err))
		}
		return true
	}
	// The comment is the whole of a not-ready verdict: without it the human never
	// sees the questions, so a failed post is retried rather than recorded.
	if err := p.Backlog.AddComment(key, notReadyComment(v, p.Cfg.Tracker.ReadyLabel)); err != nil {
		log.Event(fmt.Sprintf("%s ✗ could not comment the open questions on %s: %v — it will be judged again", who, key, err))
		return false
	}
	log.Event(fmt.Sprintf("%s — commented the open questions on %s", who, key))
	return true
}
