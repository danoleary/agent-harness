package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/secloop"
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
		"source: github_prs", "every open PR into main", "when conflicting", "3 attempts per PR",
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

	l.Source.Scope = config.ScopeOptedIn
	if got := describeLoop(cfg, l, limitsFrom(cfg, l)); !strings.Contains(got, `opt-in label "agent-maintained"`) {
		t.Errorf("an opted_in loop names its label:\n%s", got)
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

// securityLoop is a github_security_alerts loop as the loader resolves it.
func securityLoop() config.Loop {
	return config.Loop{
		Name:   "security-alerts",
		Source: config.LoopSource{Kind: config.SourceGitHubSecurityAlerts, Severity: []string{config.SeverityCritical, config.SeverityHigh}, MaxAttempts: 2},
		Steps: []config.Step{
			{Use: config.StepNewBranch}, {Use: config.StepFixAlerts, PromptPath: "prompts/security-fix.md", Prompt: "x"},
			{Use: config.StepGates}, {Use: config.StepOpenPR}, {Use: config.StepWatchCI}, {Use: config.StepRetro},
		},
	}
}

// A security-alerts loop's dry run names its Source with the severities and budget
// in force, and says what each of its new Steps does.
func TestDescribeLoopPrintsASecurityAlertsLoop(t *testing.T) {
	cfg := config.Config{Host: config.Host{ProjectPath: "/src/app"}, Project: config.Project{BranchPrefix: "feat"}}
	l := securityLoop()

	got := describeLoop(cfg, l, limitsFrom(cfg, l))

	for _, want := range []string{
		"loop security-alerts",
		"source: github_security_alerts", "severity critical, high", "feat/sec-", "2 attempts per fingerprint",
		"1. new_branch", "fresh from origin/main",
		"2. fix_alerts", "SecurityFix session", "body prompts/security-fix.md",
		"3. gates", "4. open_pr", "5. watch_ci", "6. retro",
		"/src/app/.agent-harness/logs/loops/security-alerts/loop.jsonl",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--dry-run missing %q:\n%s", want, got)
		}
	}
}

type fakePlanner struct {
	cands []secloop.Candidate
	err   error
}

func (p fakePlanner) Plan() ([]secloop.Candidate, error) { return p.cands, p.err }

// `loop security-alerts --dry-run` prints the Source's plan; a plan that cannot be
// read fails the dry run rather than printing an empty one.
func TestSecurityDryRunRendersTheSourcesPlan(t *testing.T) {
	l := securityLoop()
	c := secloop.Candidate{Item: secloop.Item{Group: secloop.Group{Ecosystem: "npm", Package: "lodash", Alerts: []github.Alert{{Number: 1, Patched: "4.17.21"}}}, Branch: "feat/sec-npm-lodash", Fingerprint: "abc"}}

	got, err := securityDryRun(l, fakePlanner{cands: []secloop.Candidate{c}})

	if err != nil || !strings.Contains(got, "SEC-npm-lodash  feat/sec-npm-lodash") || !strings.Contains(got, "work: attempt 1 of 2") {
		t.Errorf("securityDryRun = %q, %v", got, err)
	}
	if _, err := securityDryRun(l, fakePlanner{err: errors.New("403")}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("a failed plan fails the dry run, got %v", err)
	}
}

// A real run of a security-alerts loop is refused, never handed to the ticket
// loop: its Pipeline does not exist yet.
func TestASecurityAlertsLoopIsNotYetRunnable(t *testing.T) {
	if code := runSecurityLoop(config.Config{}, securityLoop(), false); code == 0 {
		t.Error("a real run must be refused")
	}
}

// `loop <unknown-name>` is refused and names the loops the project can run; it
// never falls back to the ticket loop (#249). No name is the ticket loop, and a
// known name resolves to its loop.
func TestPickLoopRefusesANameNoLoopHas(t *testing.T) {
	cfg := config.Config{}

	if _, err := pickLoop(cfg, flags{name: "pr-docter"}); err == nil {
		t.Fatal("an unknown loop name must be refused, not run as the ticket loop")
	} else {
		for _, want := range []string{`no loop named "pr-docter"`, "tickets", "pr-doctor", "[[loops]]"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal missing %q:\n%v", want, err)
			}
		}
	}
	if l, err := pickLoop(cfg, flags{}); err != nil || l.Name != config.DefaultLoopName {
		t.Errorf("no name = %q, %v; want the ticket loop", l.Name, err)
	}
	if l, err := pickLoop(cfg, flags{name: "pr-doctor"}); err != nil || l.Name != config.PRDoctorLoopName {
		t.Errorf("pr-doctor = %q, %v", l.Name, err)
	}
}
