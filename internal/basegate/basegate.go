// Package basegate is the record of what the Gates said about an untouched base:
// one verdict per origin/main SHA, shared by every loop instance of one Consumer,
// and the finding a red base files (ADR-0036). The implementation stage reads it
// before it cuts a session from a base (#125), and a conflict resolution whose
// re-gate is red reads it to tell a red base from a red branch (#250).
package basegate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/findings"
)

// FindingKey is the dedup key of the finding a red base files. One key for every
// red base, not one per SHA: while one repair issue is open, a main that moves but
// stays red records a recurrence on it rather than filing another. A ticket
// carrying this key is that repair, so the hold never applies to it.
const FindingKey = "base-gates-red"

// RedRecheck is how long a red base verdict is trusted. A red Gate can be a flake
// that failed its confirming re-run too (#189), and a flaky red that held for the
// life of its SHA would hold every ticket until someone merged to main; a green
// verdict needs no expiry, because a flake can only make a base look red.
const RedRecheck = 30 * time.Minute

// Verdict is what the Gates said about one untouched base. Gate names the first
// Gate that failed, empty when they all passed.
type Verdict struct {
	Gate string    `json:"gate,omitempty"`
	At   time.Time `json:"at"`
}

// Green reports whether every Gate passed on the base.
func (v Verdict) Green() bool { return v.Gate == "" }

// RecheckAt is when a red verdict stops being trusted and its base is gated again.
func (v Verdict) RecheckAt() time.Time { return v.At.Add(RedRecheck) }

// Verdicts is the per-SHA record of base verdicts every loop instance of one
// Consumer shares (config.BaseVerdictsDir), so a base is gated once, not once per
// ticket cut from it. Two instances that reach an ungated base together both gate
// it; the gate slots (ADR-0024) still bound how many gate at once.
type Verdicts struct {
	dir string
	now func() time.Time
}

// For is the verdict record of cfg's Consumer.
func For(cfg config.Config) Verdicts {
	return Verdicts{dir: config.BaseVerdictsDir(cfg.ProjectPath), now: time.Now}
}

// In is a verdict record kept in dir, on the clock now; for tests.
func In(dir string, now func() time.Time) Verdicts { return Verdicts{dir: dir, now: now} }

// Read returns the recorded verdict for sha. ok is false when there is none to
// trust: never recorded, unreadable, or red for longer than RedRecheck.
func (v Verdicts) Read(sha string) (Verdict, bool) {
	raw, err := os.ReadFile(filepath.Join(v.dir, sha))
	if err != nil {
		return Verdict{}, false
	}
	var got Verdict
	if err := json.Unmarshal(raw, &got); err != nil {
		return Verdict{}, false
	}
	if !got.Green() && v.now().Sub(got.At) > RedRecheck {
		return Verdict{}, false
	}
	return got, true
}

// Write records verdict for sha, stamped now, and returns it as stamped. The
// rename makes it atomic, so a concurrent reader sees the old verdict or the new
// one, never half of one.
func (v Verdicts) Write(sha string, verdict Verdict) (Verdict, error) {
	verdict.At = v.now()
	raw, err := json.Marshal(verdict)
	if err != nil {
		return verdict, err
	}
	if err := os.MkdirAll(v.dir, 0o755); err != nil {
		return verdict, err
	}
	tmp, err := os.CreateTemp(v.dir, sha+".*.tmp")
	if err != nil {
		return verdict, err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return verdict, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return verdict, err
	}
	return verdict, os.Rename(tmp.Name(), filepath.Join(v.dir, sha))
}

// Finding is the finding a red base files: gate failed on main with nothing
// applied, and body says where the harness found it. It needs a human: the loop
// would keep selecting the held work ahead of a repair issue it filed itself, so a
// person repairs main, or dispatches the issue by hand.
func Finding(gate string, exit int, stderrTail, body string) findings.Finding {
	if stderrTail != "" {
		body += "\n\nGate stderr tail:\n\n```\n" + stderrTail + "\n```"
	}
	return findings.Finding{
		Title:      fmt.Sprintf("main fails gate %q before any change, so every ticket on it is held", gate),
		Body:       body,
		Audience:   findings.AudienceProject,
		Kind:       "base-red",
		Key:        FindingKey,
		NeedsHuman: true,
	}
}
