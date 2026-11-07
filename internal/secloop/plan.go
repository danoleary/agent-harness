package secloop

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
)

// The lease and hand-off labels when the tracker names none, as on pr-doctor.
const (
	defaultHeldLabel    = "in-progress"
	defaultBlockedLabel = "blocked"
)

// defaultGrace is how long the loop waits, after its fix merges, for Dependabot to
// close the alerts it fixed.
const defaultGrace = 24 * time.Hour

// OptionsFrom maps the Consumer's config and the resolved loop onto the Source's
// knobs. The loader has already filled in the loop's severity and budget defaults.
// On a GitHub tracker the tracker's own labels are the lease and the block, so a
// human and both PR loops read one set of labels.
func OptionsFrom(cfg config.Config, l config.Loop) Options {
	o := Options{
		BranchPrefix: cfg.BranchPrefix,
		Severities:   l.Source.Severity,
		HeldLabel:    defaultHeldLabel,
		BlockedLabel: defaultBlockedLabel,
		MaxAttempts:  l.Source.MaxAttempts,
		Grace:        defaultGrace,
		LogsDir:      filepath.Join(cfg.ProjectPath, l.LogsDir()),
	}
	if cfg.Tracker.Kind == "github" {
		if l := cfg.Tracker.InProgressLabel; l != "" {
			o.HeldLabel = l
		}
		if l := cfg.Tracker.BlockedLabel; l != "" {
			o.BlockedLabel = l
		}
	}
	return o
}

// RenderPlan prints the Source's judgement of every vulnerable package for `loop
// <name> --dry-run`: which it would work and on what branch, which it would hand
// to a human, and why it skips the rest.
func RenderPlan(cands []Candidate, maxAttempts int) string {
	if len(cands) == 0 {
		return "Security-alerts loop dry-run: no open alerts at these severities.\n"
	}
	var b strings.Builder
	b.WriteString("Security-alerts loop dry-run — nothing below was changed.\n")
	for _, c := range cands {
		fmt.Fprintf(&b, "\n%s  %s  fingerprint %s\n", c.Key(), c.Branch, c.Fingerprint)
		if len(c.Alerts) > 0 {
			fmt.Fprintf(&b, "  fixes %s\n", alertNumbers(c.Alerts))
		}
		switch {
		case c.Skip != "":
			fmt.Fprintf(&b, "  skip: %s\n", c.Skip)
		case c.Exhausted:
			fmt.Fprintf(&b, "  its %d-attempt budget is used up: would hand it to a human\n", c.Attempts)
		default:
			fmt.Fprintf(&b, "  work: attempt %d of %d — cut %s fresh from origin/main and run the steps above over it\n", c.Attempts+1, maxAttempts, c.Branch)
			if len(c.Unpatched) > 0 {
				fmt.Fprintf(&b, "  left alone: no patched version for %s\n", alertNumbers(c.Unpatched))
			}
		}
	}
	return b.String()
}
