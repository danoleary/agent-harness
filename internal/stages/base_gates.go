package stages

import (
	"fmt"
	"time"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/fix"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// baseGateStep labels the containers that run the Gates over the untouched base
// before the implementation session launches (#125).
const baseGateStep = "basegate"

// baseConfirmStep labels the containers that re-run the Gates over a base that
// just went red, so one flaky Gate step cannot hold every ticket on a green main
// for basegate.RedRecheck (#189).
const baseConfirmStep = "basegate-confirm"

// baseCheck is what gating the untouched base decided. Held is the BaseRed
// Result the stage ends on when Red; GreenAt is the base SHA a base-repair
// ticket found already green (#278), empty otherwise.
type baseCheck struct {
	Held    Result
	Red     bool
	GreenAt string
}

// checkBase runs the Gates over the worktree the stage just cut and provisioned,
// before any session touches it, and reports whether the base itself is red
// (#125). A session cut from a red base meets the red Gate mid-run and repairs it
// in its own branch, as does every sibling cut from the same base, so the second
// of those PRs to land conflicts with base. Holding the ticket instead costs one
// gate run per base SHA, which the verdict record shares across tickets and loop
// instances.
//
// The check applies only to an untouched base: a branch already carrying commits
// (a resumed ticket) or uncommitted work is past it. It is skipped for --force.
// A Gate that failed environmentally — OOM-killed or unable to launch through its
// retries, or killed at its cap — says nothing about the base, so it records
// nothing and the ticket dispatches.
//
// The repair ticket a red base files (its base-gates-red finding key) is gated
// too, but never held: it is the one change that turns main green. Its base may
// have gone green while it waited in the queue, though, and then its sessions
// spent their first turns finding that out by hand (#278). So a green base
// returns its SHA in GreenAt, for the envelope to say so, and a red one
// dispatches the repair as before, filing nothing: the ticket is the finding.
//
// A red counts only if it is red twice: the Gates run once more over the same
// base before the verdict is written, and a pass on that re-run records the base
// green (#189). A green verdict needs no confirming, since a flake can only make
// a base look red.
//
// On the first red verdict for a base it files one finding, through findingsDir,
// so a human learns main is red rather than finding an idle loop.
//
// A red base holds the ticket: Held names the base and when its verdict expires,
// so the loop can idle on it (#190).
func checkBase(h hostio.Host, cfg config.Config, log *runlog.Logger, t ticket.Ticket, force bool, findingsDir string) baseCheck {
	wt := h.Worktree()
	if force || len(cfg.Gates) == 0 || wt.CommitsAhead() > 0 || !wt.Clean() {
		return baseCheck{}
	}
	repair := findings.ExtractKey(t.Description) == basegate.FindingKey
	sha, err := wt.HeadSHA()
	if err != nil {
		log.Event("⚠ could not read the base SHA (" + err.Error() + ") — dispatching without gating the base")
		return baseCheck{}
	}
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	// green and red are the two verdicts. A repair ticket is never held: on a
	// green base its envelope says so (#278), on a red one it dispatches as the
	// work that turns main green.
	green := func() baseCheck {
		if repair {
			log.Event("tdd · base " + short + " that " + t.Identifier + " repairs is already green — dispatching it with that in its envelope, so it only adds the regression test or hardening it asks for (#278)")
			return baseCheck{GreenAt: sha}
		}
		return baseCheck{}
	}
	red := func(v basegate.Verdict) baseCheck {
		if repair {
			log.Event("tdd · base " + short + " is red, as " + t.Identifier + " reports — dispatching the repair, not holding it (#125)")
			return baseCheck{}
		}
		return baseCheck{Held: Result{OK: false, Disposition: BaseRed, HeldBase: sha, HeldUntil: v.RecheckAt()}, Red: true}
	}
	verdicts := basegate.For(cfg)
	if v, ok := verdicts.Read(sha); ok {
		if v.Green() {
			log.Event("tdd · base " + short + " passed its gates already — not gating it again (#125)")
			return green()
		}
		if !repair {
			log.Event(fmt.Sprintf("tdd ⏸ base %s failed gate %q at %s — holding %s until main moves; no session launched (#125)", short, v.Gate, v.At.Format(time.RFC3339), t.Identifier))
		}
		return red(v)
	}

	log.Event("tdd · gating the untouched base " + short + " before the session, so a red main is not repaired in every branch cut from it (#125)")
	res := fix.Env{Host: h, Cfg: cfg, Log: log, Who: "tdd"}.Gates(baseGateStep)
	if res.Outcome.Retryable() || res.Outcome.CapKilled {
		log.Event(fmt.Sprintf("⚠ base gate %q failed environmentally (exit %d: an OOM-kill, a failed launch or its cap) — that says nothing about the base; dispatching", res.FailedGate, res.Outcome.ExitCode))
		return baseCheck{}
	}
	if !res.Green() {
		log.Event(fmt.Sprintf("tdd ↻ base gate %q is red (exit %d) — re-running the gates once before the base counts as red (#189)", res.FailedGate, res.Outcome.ExitCode))
		first := res
		res = fix.Env{Host: h, Cfg: cfg, Log: log, Who: "tdd"}.Gates(baseConfirmStep)
		if res.Outcome.Retryable() || res.Outcome.CapKilled {
			log.Event(fmt.Sprintf("⚠ base gate %q failed environmentally on its re-run (exit %d) — the red is unconfirmed; dispatching", res.FailedGate, res.Outcome.ExitCode))
			return baseCheck{}
		}
		if res.Green() {
			log.Event(fmt.Sprintf("tdd · base gate %q flaked: red (exit %d), then green on its re-run — recording the base green (#189)", first.FailedGate, first.Outcome.ExitCode))
		}
	}
	verdict, err := verdicts.Write(sha, basegate.Verdict{Gate: res.FailedGate})
	if err != nil {
		log.Event("⚠ could not record the base verdict (" + err.Error() + ") — the next ticket on this base gates it again")
	}
	if verdict.Green() {
		log.Event("tdd ✓ base " + short + " passes its gates")
		return green()
	}
	if repair {
		return red(verdict)
	}
	log.Event(fmt.Sprintf("tdd ⏸ base %s fails gate %q (exit %d) before any change — holding %s until main moves; no session launched (#125)", short, res.FailedGate, res.Outcome.ExitCode, t.Identifier))
	// What the gate printed, and where its step log is, so the reader can tell
	// why main is red without opening a second file (#176).
	if s := redGateNarration(log.Dir, res); s != "" {
		log.Event("tdd · base " + s)
	}
	fileBaseRed(h, log, t, sha, res, findingsDir)
	return red(verdict)
}

// fileBaseRed files the finding a red base raises (basegate.Finding).
func fileBaseRed(h hostio.Host, log *runlog.Logger, t ticket.Ticket, sha string, res fix.GateResult, findingsDir string) {
	body := fmt.Sprintf(
		"The harness gated `main` at `%s` before starting an implementation session, and gate `%s` failed (exit %d) with no change applied.\n\n"+
			"Every ticket cut from this base is held: no session launches until `main` moves, so the red gate is not repaired as an out-of-scope change in each feature branch (#125). The ticket that hit it was %s.\n\n"+
			"Repair `main` directly, or dispatch this issue by hand (`agent-harness pipeline <this issue>`): an issue carrying this finding key is exempt from the hold.",
		sha, res.FailedGate, res.Outcome.ExitCode, t.Identifier,
	)
	f := basegate.Finding(res.FailedGate, res.Outcome.ExitCode, res.Outcome.StderrTail, body)
	if err := filing.WriteDropbox(findingsDir, []findings.Finding{f}); err != nil {
		log.Event("⚠ could not write the red-base finding (" + err.Error() + ")")
		return
	}
	h.FileFindings(findingsDir, t.TeamID, t.Identifier)
}
