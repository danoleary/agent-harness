package stages

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// baseRedFindingKey is the dedup key of the finding a red base files. One key for
// every red base, not one per SHA: while one repair issue is open, a main that moves
// but stays red records a recurrence on it rather than filing another. A ticket
// carrying this key is that repair, so the hold never applies to it.
const baseRedFindingKey = "base-gates-red"

// baseRedRecheck is how long a red base verdict is trusted. A red Gate can be a
// flake, and a flaky red that held for the life of its SHA would hold every ticket
// until someone merged to main; a green verdict needs no expiry, because a flake
// can only make a base look red.
const baseRedRecheck = 30 * time.Minute

// baseVerdict is what the Gates said about one untouched base. Gate names the
// first Gate that failed, empty when they all passed.
type baseVerdict struct {
	Gate string    `json:"gate,omitempty"`
	At   time.Time `json:"at"`
}

// Green reports whether every Gate passed on the base.
func (v baseVerdict) Green() bool { return v.Gate == "" }

// baseVerdicts is the per-SHA record of base verdicts every loop instance of one
// Consumer shares (config.BaseVerdictsDir), so a base is gated once, not once per
// ticket cut from it. Two instances that reach an ungated base together both gate
// it; the gate slots (ADR-0024) still bound how many gate at once.
type baseVerdicts struct {
	dir string
	now func() time.Time
}

func newBaseVerdicts(cfg config.Config) baseVerdicts {
	return baseVerdicts{dir: config.BaseVerdictsDir(cfg.ProjectPath), now: time.Now}
}

// read returns the recorded verdict for sha. ok is false when there is none to
// trust: never recorded, unreadable, or red for longer than baseRedRecheck.
func (v baseVerdicts) read(sha string) (baseVerdict, bool) {
	raw, err := os.ReadFile(filepath.Join(v.dir, sha))
	if err != nil {
		return baseVerdict{}, false
	}
	var got baseVerdict
	if err := json.Unmarshal(raw, &got); err != nil {
		return baseVerdict{}, false
	}
	if !got.Green() && v.now().Sub(got.At) > baseRedRecheck {
		return baseVerdict{}, false
	}
	return got, true
}

// write records verdict for sha, stamped now. The rename makes it atomic, so a
// concurrent reader sees the old verdict or the new one, never half of one.
func (v baseVerdicts) write(sha string, verdict baseVerdict) error {
	verdict.At = v.now()
	raw, err := json.Marshal(verdict)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(v.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(v.dir, sha+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(v.dir, sha))
}

// baseIsRed runs the Gates over the worktree the stage just cut and provisioned,
// before any session touches it, and reports whether the base itself is red
// (#125). A session cut from a red base meets the red Gate mid-run and repairs it
// in its own branch, as does every sibling cut from the same base, so the second
// of those PRs to land conflicts with base. Holding the ticket instead costs one
// gate run per base SHA, which the verdict record shares across tickets and loop
// instances.
//
// The check applies only to an untouched base: a branch already carrying commits
// (a resumed ticket) or uncommitted work is past it. It is skipped for --force,
// and for the repair ticket a red base files, which is the one change that turns
// main green. A Gate that failed environmentally — OOM-killed or unable to launch
// through its retries, or killed at its cap — says nothing about the base, so it
// records nothing and the ticket dispatches.
//
// On the first red verdict for a base it files one finding, through findingsDir,
// so a human learns main is red rather than finding an idle loop.
func baseIsRed(h hostio.Host, cfg config.Config, log *runlog.Logger, t ticket.Ticket, force bool, findingsDir string) bool {
	wt := h.Worktree()
	switch {
	case force, len(cfg.Gates) == 0:
		return false
	case findings.ExtractKey(t.Description) == baseRedFindingKey:
		log.Event("tdd · " + t.Identifier + " repairs a red base — not gating the base first (#125)")
		return false
	case wt.CommitsAhead() > 0 || !wt.Clean():
		return false
	}
	sha, err := wt.HeadSHA()
	if err != nil {
		log.Event("⚠ could not read the base SHA (" + err.Error() + ") — dispatching without gating the base")
		return false
	}
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	verdicts := newBaseVerdicts(cfg)
	if v, ok := verdicts.read(sha); ok {
		if v.Green() {
			log.Event("tdd · base " + short + " passed its gates already — not gating it again (#125)")
			return false
		}
		log.Event(fmt.Sprintf("tdd ⏸ base %s failed gate %q at %s — holding %s until main moves; no session launched (#125)", short, v.Gate, v.At.Format(time.RFC3339), t.Identifier))
		return true
	}

	log.Event("tdd · gating the untouched base " + short + " before the session, so a red main is not repaired in every branch cut from it (#125)")
	res := fix.Env{Host: h, Cfg: cfg, Log: log, Who: "tdd"}.Gates(baseGateStep)
	if res.Outcome.Retryable() || res.Outcome.CapKilled {
		log.Event(fmt.Sprintf("⚠ base gate %q failed environmentally (exit %d: an OOM-kill, a failed launch or its cap) — that says nothing about the base; dispatching", res.FailedGate, res.Outcome.ExitCode))
		return false
	}
	verdict := baseVerdict{Gate: res.FailedGate}
	if err := verdicts.write(sha, verdict); err != nil {
		log.Event("⚠ could not record the base verdict (" + err.Error() + ") — the next ticket on this base gates it again")
	}
	if verdict.Green() {
		log.Event("tdd ✓ base " + short + " passes its gates")
		return false
	}
	log.Event(fmt.Sprintf("tdd ⏸ base %s fails gate %q (exit %d) before any change — holding %s until main moves; no session launched (#125)", short, res.FailedGate, res.Outcome.ExitCode, t.Identifier))
	if res.Outcome.StderrTail != "" {
		log.Event("tdd · base gate stderr tail:\n" + res.Outcome.StderrTail)
	}
	fileBaseRed(h, log, t, sha, res, findingsDir)
	return true
}

// fileBaseRed files the finding a red base raises. It needs a human: the loop
// would keep selecting the held tickets ahead of a repair issue it filed itself,
// so a person repairs main, or dispatches the issue by hand.
func fileBaseRed(h hostio.Host, log *runlog.Logger, t ticket.Ticket, sha string, res fix.GateResult, findingsDir string) {
	body := fmt.Sprintf(
		"The harness gated `main` at `%s` before starting an implementation session, and gate `%s` failed (exit %d) with no change applied.\n\n"+
			"Every ticket cut from this base is held: no session launches until `main` moves, so the red gate is not repaired as an out-of-scope change in each feature branch (#125). The ticket that hit it was %s.\n\n"+
			"Repair `main` directly, or dispatch this issue by hand (`agent-harness pipeline <this issue>`): an issue carrying this finding key is exempt from the hold.",
		sha, res.FailedGate, res.Outcome.ExitCode, t.Identifier,
	)
	if res.Outcome.StderrTail != "" {
		body += "\n\nGate stderr tail:\n\n```\n" + res.Outcome.StderrTail + "\n```"
	}
	f := findings.Finding{
		Title:      fmt.Sprintf("main fails gate %q before any change, so every ticket on it is held", res.FailedGate),
		Body:       body,
		Audience:   findings.AudienceProject,
		Kind:       "base-red",
		Key:        baseRedFindingKey,
		NeedsHuman: true,
	}
	if err := filing.WriteDropbox(findingsDir, []findings.Finding{f}); err != nil {
		log.Event("⚠ could not write the red-base finding (" + err.Error() + ")")
		return
	}
	h.FileFindings(findingsDir, t.TeamID, t.Identifier)
}
