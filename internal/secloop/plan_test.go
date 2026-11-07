package secloop

import (
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
)

// The dry-run prints every package the Source judged: the one it would work, with
// its alerts, branch, fingerprint and attempt; the one it would hand to a human;
// and why it skips the rest.
func TestRenderPlanSaysWhatTheLoopWouldDoWithEachPackage(t *testing.T) {
	f := newFixture(t,
		lodash(3), lodash(1), npmAlert(2, "lodash", "pnpm-lock.yaml", ""),
		npmAlert(5, "minimist", "pnpm-lock.yaml", ""),
		github.Alert{Number: 7, Ecosystem: "pip", Package: "requests", ManifestPath: "requirements.txt", Patched: "2.32.0"},
	)

	got := RenderPlan(f.plan(t), 2)

	lodashItem := Group{Ecosystem: "npm", Package: "lodash", Alerts: []github.Alert{lodash(1), lodash(3)}}
	for _, want := range []string{
		"nothing below was changed",
		"SEC-npm-lodash  feat/sec-npm-lodash  fingerprint " + Fingerprint(lodashItem),
		"alerts #1, #3",
		"work: attempt 1 of 2",
		"left alone: no patched version for alert #2",
		"SEC-npm-minimist", "skip: no patched version for alert #5",
		"SEC-pip-requests", "skip: ecosystem pip is not supported",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan missing %q:\n%s", want, got)
		}
	}
}

// A fingerprint at its budget is shown as handed to a human, not worked.
func TestRenderPlanHandsAnExhaustedFingerprintToAHuman(t *testing.T) {
	c := Candidate{Item: Item{Group: Group{Ecosystem: "npm", Package: "lodash", Alerts: []github.Alert{lodash(1)}}, Branch: "feat/sec-npm-lodash", Fingerprint: "abc"}, Attempts: 2, Exhausted: true}

	got := RenderPlan([]Candidate{c}, 2)

	if !strings.Contains(got, "its 2-attempt budget is used up: would hand it to a human") || strings.Contains(got, "work:") {
		t.Errorf("an exhausted fingerprint is handed off:\n%s", got)
	}
	if got := RenderPlan(nil, 2); !strings.Contains(got, "no open alerts") {
		t.Errorf("an empty plan says so, got %q", got)
	}
}

// The Source runs with the loop's severities and budget, the Consumer's
// branch_prefix, the tracker's labels on a GitHub tracker, and its ledger under
// the loop's own logs dir in the checkout.
func TestOptionsFromTheResolvedLoop(t *testing.T) {
	cfg := config.Config{Host: config.Host{ProjectPath: "/src/app"}, Project: config.Project{BranchPrefix: "feat"}}
	l := config.Loop{Name: "security-alerts", Source: config.LoopSource{Kind: config.SourceGitHubSecurityAlerts, Severity: []string{"critical"}, MaxAttempts: 3}}

	o := OptionsFrom(cfg, l)

	if o.BranchPrefix != "feat" || o.MaxAttempts != 3 || len(o.Severities) != 1 || o.Severities[0] != "critical" {
		t.Errorf("OptionsFrom = %+v", o)
	}
	if o.HeldLabel != "in-progress" || o.BlockedLabel != "blocked" || o.Grace != 24*time.Hour {
		t.Errorf("defaults = %+v", o)
	}
	if o.LogsDir != "/src/app/.agent-harness/logs/loops/security-alerts" {
		t.Errorf("LogsDir = %q", o.LogsDir)
	}

	cfg.Tracker = config.TrackerConfig{Kind: "github", InProgressLabel: "agent-working", BlockedLabel: "needs-human"}
	if o := OptionsFrom(cfg, l); o.HeldLabel != "agent-working" || o.BlockedLabel != "needs-human" {
		t.Errorf("a GitHub tracker's labels win, got %+v", o)
	}
}
