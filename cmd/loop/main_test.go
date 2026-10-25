package main

import (
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
)

// `loop` runs the ticket loop, `loop <name>` the named loop, and --list and
// --dry-run print without running anything. --instance <id> names one of several
// daemons running the loop (#89); its value is never taken for a loop name. --prs, the PR loop's old flag, is
// refused with its replacement: ignoring it would start a real ticket loop.
func TestParseFlags(t *testing.T) {
	cases := []struct {
		argv    []string
		want    flags
		wantErr string
	}{
		{argv: nil, want: flags{}},
		{argv: []string{"pr-doctor"}, want: flags{name: "pr-doctor"}},
		{argv: []string{"pr-doctor", "--dry-run"}, want: flags{name: "pr-doctor", dryRun: true}},
		{argv: []string{"--dry-run", "--verbose", "tickets"}, want: flags{name: "tickets", dryRun: true}},
		{argv: []string{"--list"}, want: flags{list: true}},
		{argv: []string{"--dry-run"}, want: flags{dryRun: true}},
		{argv: []string{"--prs"}, wantErr: "agent-harness loop pr-doctor"},
		{argv: []string{"tickets", "pr-doctor"}, wantErr: "one loop at a time"},
		{argv: []string{"--instance", "b"}, want: flags{instance: "b"}},
		{argv: []string{"--instance=b", "tickets"}, want: flags{name: "tickets", instance: "b"}},
		{argv: []string{"tickets", "--instance", "b", "--dry-run"}, want: flags{name: "tickets", instance: "b", dryRun: true}},
		{argv: []string{"--instance"}, wantErr: "--instance needs an id"},
		{argv: []string{"--instance", "--dry-run"}, wantErr: "--instance needs an id"},
	}
	for _, c := range cases {
		got, err := parseFlags(c.argv)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("parseFlags(%v) error = %v, want one mentioning %q", c.argv, err, c.wantErr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseFlags(%v) = %+v, %v; want %+v", c.argv, got, err, c.want)
		}
	}
}

// A loop's own limits override the LOOP_* env knobs; a limit it leaves unset keeps
// the knob's value.
func TestLimitsFromPrefersTheLoopsOwnLimits(t *testing.T) {
	cfg := config.Config{Host: config.Host{LoopPollInterval: time.Minute, LoopCapBackoff: 45 * time.Minute, LoopClaimTTL: 30 * time.Minute, LoopMaxConsecutiveFailures: 3, LoopMaxTickets: 0}}
	l := config.Loop{Name: "pr-doctor", Limits: config.LoopLimits{PollInterval: 10 * time.Minute, MaxConsecutiveFailures: 5, MaxItems: 7}}

	got := limitsFrom(cfg, l)

	if got.PollInterval != 10*time.Minute || got.MaxConsecutiveFailures != 5 || got.MaxTickets != 7 {
		t.Errorf("the loop's own limits must win, got %+v", got)
	}
	if got.CapBackoff != 45*time.Minute || got.ClaimTTL != 30*time.Minute || got.TickInterval != tickInterval {
		t.Errorf("unset limits keep the env knobs, got %+v", got)
	}
}

// --list names every loop the project can run, its Source and Steps, and which
// are built in.
func TestRenderListNamesEveryLoop(t *testing.T) {
	got := renderList(config.Project{}.AllLoops())
	for _, want := range []string{
		"tickets", "(default, built in)", "tracker", "implement → review → watch_ci → retro",
		"pr-doctor", "github_prs", "checkout_pr → fix → gates → push_existing → watch_ci → retro",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--list missing %q:\n%s", want, got)
		}
	}
}

// --dry-run prints the named loop as it resolved: its Source with the parameters
// in force, each Step with what it checks and any prompt body it supplies, the
// limits, its sentinel and its stream.
func TestDescribeLoopPrintsTheResolvedSourceAndSteps(t *testing.T) {
	cfg := config.Config{
		Host:    config.Host{ProjectPath: "/src/app", PRLoopLabel: "agent-maintained", PRLoopMaxAttempts: 3, LoopPollInterval: time.Minute, LoopCapBackoff: time.Hour, LoopMaxConsecutiveFailures: 3, LoopClaimTTL: 30 * time.Minute},
		Project: config.Project{BranchPrefix: "feat"},
	}
	l := config.Loop{
		Name:   "conflicts",
		Source: config.LoopSource{Kind: config.SourceGitHubPRs, When: []string{config.WhenConflicting}},
		Steps: []config.Step{
			{Use: config.StepCheckoutPR}, {Use: config.StepFix, PromptPath: "prompts/fix-pr.md", Prompt: "x"},
			{Use: config.StepGates}, {Use: config.StepPushExisting},
		},
		Limits: config.LoopLimits{PollInterval: 10 * time.Minute},
	}

	got := describeLoop(cfg, l, limitsFrom(cfg, l))

	for _, want := range []string{
		"loop conflicts", "nothing below was changed",
		"source: github_prs", `opt-in label "agent-maintained"`, "when conflicting", "3 attempts per PR",
		"1. checkout_pr", "2. fix", "body prompts/fix-pr.md", "3. gates", "4. push_existing",
		"poll every 10m0s",
		"/src/app/.agent-harness/STOP-conflicts", "/src/app/.agent-harness/logs/loops/conflicts/loop.jsonl",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--dry-run missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "watch_ci") {
		t.Errorf("a step the loop leaves out must not be printed:\n%s", got)
	}
}

// A named instance's dry run prints the sentinel that stops it alone, beside the
// shared ones, and its own stream (#89).
func TestDescribeLoopNamesTheInstancesOwnSentinelAndStream(t *testing.T) {
	cfg := config.Config{Host: config.Host{ProjectPath: "/src/app", StopFile: ".agent-harness/STOP"}}
	l, err := config.Project{}.LoopInstance("", "b")
	if err != nil {
		t.Fatal(err)
	}

	got := describeLoop(cfg, l, limitsFrom(cfg, l))

	for _, want := range []string{
		"loop tickets, instance b",
		"/src/app/.agent-harness/STOP-b",
		"/src/app/.agent-harness/STOP-tickets",
		"/src/app/.agent-harness/logs/instances/b/loop.jsonl",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--dry-run missing %q:\n%s", want, got)
		}
	}
}
