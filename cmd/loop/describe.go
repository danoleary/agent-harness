package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/prloop"
)

// stepChecks says, for the dry-run, what each Step does and the ground truth it
// checks before the next one runs.
var stepChecks = map[string]string{
	config.StepImplement:    "a /tdd session into a fresh worktree; checks a committed handoff on the branch",
	config.StepReview:       "a cold review, then every gate host-side; lands the branch as a PR only when they are green",
	config.StepRetro:        "mines the run's transcripts and files its findings",
	config.StepCheckoutPR:   "checks the PR's head branch out at the polled head and runs post_create",
	config.StepFix:          "a RebaseFix session over a conflict; checks a clean tree with origin/main an ancestor of the tip",
	config.StepGates:        "re-runs every gate over the fixed tree; nothing is pushed unless all are green",
	config.StepPushExisting: "pushes to the PR's existing branch: force-with-lease after a rebase, plain after a merge",
	config.StepWatchCI:      "watches CI to a verdict; a red check gets a CIFix session whose commit must pass every gate before it is pushed",
}

// renderList prints every loop `agent-harness loop <name>` can run.
func renderList(loops []config.Loop) string {
	var b strings.Builder
	b.WriteString("Loops this project can run (agent-harness loop <name>):\n")
	for _, l := range loops {
		var tags []string
		if l.Name == config.DefaultLoopName {
			tags = append(tags, "default")
		}
		if l.BuiltIn {
			tags = append(tags, "built in")
		} else {
			tags = append(tags, "declared in config.toml")
		}
		fmt.Fprintf(&b, "  %-12s (%s)  source %s: %s\n", l.Name, strings.Join(tags, ", "), l.Source.Kind, strings.Join(l.StepNames(), " → "))
	}
	return b.String()
}

// describeLoop prints the loop as it resolved for `loop <name> --dry-run`: its
// Source with the parameters in force, its Steps in order, the limits the daemon
// will run with, and where its sentinel and stream are.
func describeLoop(cfg config.Config, l config.Loop, limits loop.Limits) string {
	var b strings.Builder
	origin := "declared in config.toml"
	if l.BuiltIn {
		origin = "built in"
	}
	name := l.Name
	if l.Instance != "" {
		name += ", instance " + l.Instance
	}
	fmt.Fprintf(&b, "loop %s (%s) — dry run, nothing below was changed.\n\n", name, origin)
	fmt.Fprintf(&b, "source: %s\n", describeSource(cfg, l))
	b.WriteString("steps:\n")
	for i, s := range l.Steps {
		fmt.Fprintf(&b, "  %d. %-13s %s", i+1, s.Use, stepChecks[s.Use])
		if s.PromptPath != "" {
			fmt.Fprintf(&b, " (Project body %s)", s.PromptPath)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "limits: poll every %s, cap backoff %s, breaker after %d consecutive failures, %s, %s, claim TTL %s\n",
		limits.PollInterval, limits.CapBackoff, limits.MaxConsecutiveFailures,
		ceiling("items", limits.MaxTickets), runtime(limits.MaxRuntime), limits.ClaimTTL)
	if own := l.InstanceStopFile(); own != "" {
		fmt.Fprintf(&b, "stop:   touch %s (or %s for every instance of the loop, %s for every loop)\n",
			filepath.Join(cfg.ProjectPath, own), filepath.Join(cfg.ProjectPath, l.StopFile()), underCheckout(cfg, cfg.StopFile))
	} else {
		fmt.Fprintf(&b, "stop:   touch %s (or %s for every loop)\n",
			filepath.Join(cfg.ProjectPath, l.StopFile()), underCheckout(cfg, cfg.StopFile))
	}
	fmt.Fprintf(&b, "stream: %s\n", loopstream.PathUnder(filepath.Join(cfg.ProjectPath, l.LogsDir())))
	return b.String()
}

// describeSource names the Source and the parameters it will run with, the env
// knobs filled in where the loop sets none.
func describeSource(cfg config.Config, l config.Loop) string {
	switch l.Source.Kind {
	case config.SourceGitHubPRs:
		o := prloop.OptionsFrom(cfg, l)
		return fmt.Sprintf("github_prs — open PRs into %s on %s/ or with opt-in label %q, when %s; %d attempts per PR",
			o.BaseBranch, o.BranchPrefix, o.OptInLabel, strings.Join(l.Source.When, ", "), o.MaxAttempts)
	default:
		desc := fmt.Sprintf("tracker — the %s ready queue", cfg.Tracker.Kind)
		if hint := cfg.Tracker.QueueHint(); hint != "" {
			desc += " (" + hint + ")"
		}
		return desc
	}
}

// underCheckout resolves a relative path against the checkout, as the daemon does.
func underCheckout(cfg config.Config, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(cfg.ProjectPath, path)
}

func ceiling(what string, n int) string {
	if n <= 0 {
		return "no " + what + " ceiling"
	}
	return fmt.Sprintf("at most %d %s", n, what)
}

func runtime(d time.Duration) string {
	if d <= 0 {
		return "no runtime ceiling"
	}
	return "at most " + d.String() + " runtime"
}
