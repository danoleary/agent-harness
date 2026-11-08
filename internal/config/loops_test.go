package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// loopsCheckout is a Consumer checkout whose config is testConfigTOML plus loops.
func loopsCheckout(t *testing.T, loops string) string {
	t.Helper()
	return testCheckout(t, testConfigTOML+loops)
}

// With no [[loops]], the four built-in loops are what `loop` can run: the ticket
// loop, which a bare `agent-harness loop` runs as it always has, pr-doctor,
// triage and security-alerts.
func TestNoLoopsDeclaredLeavesTheBuiltIns(t *testing.T) {
	pc, err := LoadProject(loopsCheckout(t, ""))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	p := Project{Loops: pc.ResolvedLoops}
	got := p.AllLoops()
	if len(got) != 4 || got[0].Name != DefaultLoopName || got[1].Name != PRDoctorLoopName || got[2].Name != TriageLoopName || got[3].Name != SecurityAlertsLoopName {
		t.Fatalf("AllLoops = %+v, want the built-in tickets, pr-doctor, triage and security-alerts loops", got)
	}
	tickets, ok := p.LoopNamed("")
	if !ok || tickets.Name != DefaultLoopName || tickets.Source.Kind != SourceTracker {
		t.Errorf("the unnamed loop is the ticket loop, got %+v", tickets)
	}
	if steps := tickets.StepNames(); !reflect.DeepEqual(steps, []string{StepImplement, StepReview, StepWatchCI, StepRetro}) {
		t.Errorf("ticket loop steps = %v", steps)
	}
	if tickets.LeavesCIToPRLoop() {
		t.Error("the built-in ticket loop watches its own PR's CI, as it always has (#110)")
	}
	pr, _ := p.LoopNamed(PRDoctorLoopName)
	if pr.Source.Kind != SourceGitHubPRs || !reflect.DeepEqual(pr.StepNames(), []string{StepCheckoutPR, StepFix, StepGates, StepPushExisting, StepWatchCI, StepRetro}) {
		t.Errorf("pr-doctor = %+v", pr)
	}
	if !pr.Source.WorksAllPRs() {
		t.Error("the built-in pr-doctor works every open PR by default")
	}
	sec, _ := p.LoopNamed(SecurityAlertsLoopName)
	if sec.Source.Kind != SourceGitHubSecurityAlerts || !reflect.DeepEqual(sec.StepNames(), []string{StepNewBranch, StepFixAlerts, StepGates, StepOpenPR, StepWatchCI, StepRetro}) {
		t.Errorf("security-alerts = %+v", sec)
	}
	if !reflect.DeepEqual(sec.Source.Severity, []string{SeverityCritical, SeverityHigh}) || sec.Source.MaxAttempts != defaultSecurityMaxAttempts {
		t.Errorf("the built-in security-alerts loop has the declared loop's defaults, got %+v", sec.Source)
	}
	if _, ok := p.LoopNamed("nope"); ok {
		t.Error("an undeclared name is not a loop")
	}
}

// A ticket loop written without watch_ci leaves its PR's CI to a PR loop (#110):
// it is a valid loop, and the review stage returns once the PR is open. A PR loop
// never leaves CI to itself.
func TestATicketLoopWithoutWatchCILeavesCIToThePRLoop(t *testing.T) {
	pc, err := LoadProject(loopsCheckout(t, `
[[loops]]
name = "tickets"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }, { use = "retro" }]
`))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	p := Project{Loops: pc.ResolvedLoops}
	tickets, _ := p.LoopNamed("")
	if !tickets.LeavesCIToPRLoop() {
		t.Error("a ticket loop without watch_ci leaves CI to the PR loop")
	}
	pr, _ := p.LoopNamed(PRDoctorLoopName)
	if pr.LeavesCIToPRLoop() {
		t.Error("only a ticket loop hands its CI on")
	}
}

// A github_prs loop works every open PR unless it says otherwise: a scope of
// opted_in, or a label (which only means something under opt-in).
func TestAGitHubPRsLoopScopesToAllPRsByDefault(t *testing.T) {
	for _, tc := range []struct {
		source string
		scope  string
	}{
		{`{ kind = "github_prs", when = ["conflicting"] }`, ScopeAll},
		{`{ kind = "github_prs", when = ["conflicting"], scope = "all" }`, ScopeAll},
		{`{ kind = "github_prs", when = ["conflicting"], scope = "opted_in" }`, ScopeOptedIn},
		{`{ kind = "github_prs", when = ["conflicting"], label = "rebase-me" }`, ScopeOptedIn},
	} {
		pc, err := LoadProject(loopsCheckout(t, `
[[loops]]
name = "prs"
source = `+tc.source+`
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates" }, { use = "push_existing" }]
`))
		if err != nil {
			t.Fatalf("%s: LoadProject: %v", tc.source, err)
		}
		l, _ := Project{Loops: pc.ResolvedLoops}.LoopNamed("prs")
		if l.Source.Scope != tc.scope || l.Source.WorksAllPRs() != (tc.scope == ScopeAll) {
			t.Errorf("%s: scope = %q, want %q", tc.source, l.Source.Scope, tc.scope)
		}
	}
}

// builtInLoopsTOML writes the four built-in loops out as [[loops]] entries.
const builtInLoopsTOML = `
[[loops]]
name = "tickets"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }, { use = "watch_ci" }, { use = "retro" }]

[[loops]]
name = "pr-doctor"
source = { kind = "github_prs", when = ["conflicting", "ci_failing"] }
steps = [
  { use = "checkout_pr" },
  { use = "fix" },
  { use = "gates" },
  { use = "push_existing" },
  { use = "watch_ci" },
  { use = "retro" },
]

[[loops]]
name = "triage"
source = { kind = "backlog" }
steps = [{ use = "assess" }, { use = "apply" }]

[[loops]]
name = "security-alerts"
source = { kind = "github_security_alerts" }
steps = [
  { use = "new_branch" },
  { use = "fix_alerts" },
  { use = "gates" },
  { use = "open_pr" },
  { use = "watch_ci" },
  { use = "retro" },
]
`

// Each built-in loop can be written out as a [[loops]] entry, and the entry
// resolves to the same loop the built-in wiring runs.
func TestTheBuiltInLoopsWrittenAsConfigResolveTheSame(t *testing.T) {
	pc, err := LoadProject(loopsCheckout(t, builtInLoopsTOML))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	declared := Project{Loops: pc.ResolvedLoops}.AllLoops()
	builtIn := Project{}.AllLoops()
	if len(declared) != len(builtIn) {
		t.Fatalf("declared %d loops, built-in %d", len(declared), len(builtIn))
	}
	for i := range builtIn {
		d, b := declared[i], builtIn[i]
		if d.BuiltIn || !b.BuiltIn {
			t.Errorf("%s: BuiltIn = %v / %v", d.Name, d.BuiltIn, b.BuiltIn)
		}
		d.BuiltIn = b.BuiltIn
		if !reflect.DeepEqual(d, b) {
			t.Errorf("declared %s resolves to\n  %+v\nwant the built-in\n  %+v", d.Name, d, b)
		}
	}
}

// A loop's parameters reach the resolved loop: the source's opt-in label, its
// failure kinds and budget, a step's prompt body, and the per-loop limits.
func TestADeclaredLoopCarriesItsParameters(t *testing.T) {
	dir := loopsCheckout(t, `
[[loops]]
name = "conflicts"
poll_interval = "10m"
cap_backoff = "1h"
claim_ttl = "45m"
max_runtime = "8h"
max_consecutive_failures = 5
max_items = 20
source = { kind = "github_prs", label = "bot-fix", when = ["conflicting"], max_attempts = 2 }
steps = [
  { use = "checkout_pr" },
  { use = "fix", prompt = "prompts/fix-pr.md" },
  { use = "gates" },
  { use = "push_existing" },
]
`)
	writePromptBody(t, dir, "fix-pr", "Follow CONTRIBUTING.md.")
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	l, ok := Project{Loops: pc.ResolvedLoops}.LoopNamed("conflicts")
	if !ok {
		t.Fatal("conflicts is not declared")
	}
	want := LoopLimits{PollInterval: 10 * time.Minute, CapBackoff: time.Hour, ClaimTTL: 45 * time.Minute, MaxRuntime: 8 * time.Hour, MaxConsecutiveFailures: 5, MaxItems: 20}
	if l.Limits != want {
		t.Errorf("Limits = %+v, want %+v", l.Limits, want)
	}
	if l.Source.Label != "bot-fix" || l.Source.MaxAttempts != 2 || !reflect.DeepEqual(l.Source.When, []string{WhenConflicting}) {
		t.Errorf("Source = %+v", l.Source)
	}
	if l.Prompt(StepFix) != "Follow CONTRIBUTING.md." || l.Prompt(StepGates) != "" {
		t.Errorf("Prompt(fix) = %q", l.Prompt(StepFix))
	}
	if l.Has(StepWatchCI) || !l.Has(StepPushExisting) {
		t.Errorf("Has: steps are %v", l.StepNames())
	}
	if l.StopFile() != ".agent-harness/STOP-conflicts" {
		t.Errorf("StopFile = %q", l.StopFile())
	}
}

// A declared loop that takes a built-in's name replaces it, so a Consumer can
// re-parameterise the ticket loop a bare `agent-harness loop` runs.
func TestADeclaredLoopReplacesTheBuiltInOfTheSameName(t *testing.T) {
	dir := loopsCheckout(t, `
[[loops]]
name = "tickets"
source = { kind = "tracker" }
steps = [{ use = "implement", prompt = "prompts/implement-strict.md" }, { use = "review" }]
`)
	writePromptBody(t, dir, "implement-strict", "/tdd, strictly")
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	all := Project{Loops: pc.ResolvedLoops}.AllLoops()
	if len(all) != 4 {
		t.Fatalf("AllLoops = %+v, want tickets and the built-in pr-doctor, triage and security-alerts", all)
	}
	l, _ := Project{Loops: pc.ResolvedLoops}.LoopNamed("")
	if l.BuiltIn || l.Has(StepRetro) || l.Prompt(StepImplement) != "/tdd, strictly" {
		t.Errorf("the declared tickets loop must replace the built-in, got %+v", l)
	}
}

// Every loop problem is reported in one error with its file:line:column, beside
// the rest of the config's problems: an unknown source, an unknown step, a bad
// step order and a missing prompt file.
func TestLoadProjectReportsEveryLoopProblemTogether(t *testing.T) {
	dir := loopsCheckout(t, `
[[loops]]
name = "queue"
source = { kind = "jira_board" }
steps = [{ use = "implement" }, { use = "review" }]

[[loops]]
name = "prs"
source = { kind = "github_prs", when = ["conflicting"] }
steps = [
  { use = "checkout_pr" },
  { use = "fix", prompt = "prompts/missing.md" },
  { use = "push_existing" },
  { use = "gates" },
  { use = "deploy" },
]
`)
	_, err := LoadProject(dir)
	assertMentions(t, err,
		`config.toml:25:12: loops[0] ("queue"): unknown source kind "jira_board"`,
		`config.toml:33:3: loops[1] ("prs") steps[1]: prompt file prompts/missing.md`,
		`config.toml:34:3: loops[1] ("prs") steps[2]: push_existing needs gates before it`,
		`config.toml:36:3: loops[1] ("prs") steps[4]: unknown step "deploy"`,
		"4 problems",
	)
}

// A github_security_alerts loop resolves with its defaults filled in: the
// critical and high alerts, two attempts per fingerprint. Its fix_alerts step
// supplies the SecurityFix body.
func TestAGitHubSecurityAlertsLoopResolvesWithItsDefaults(t *testing.T) {
	dir := loopsCheckout(t, `
[[loops]]
name = "security-alerts"
source = { kind = "github_security_alerts" }
steps = [
  { use = "new_branch" },
  { use = "fix_alerts", prompt = "prompts/security-fix.md" },
  { use = "gates" },
  { use = "open_pr" },
  { use = "watch_ci" },
  { use = "retro" },
]

[[loops]]
name = "everything"
source = { kind = "github_security_alerts", severity = ["low", "medium", "high", "critical"], max_attempts = 4 }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`)
	writePromptBody(t, dir, "security-fix", "Prefer an override.")
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	p := Project{Loops: pc.ResolvedLoops}
	l, ok := p.LoopNamed("security-alerts")
	if !ok {
		t.Fatal("security-alerts is not declared")
	}
	want := LoopSource{Kind: SourceGitHubSecurityAlerts, Severity: []string{SeverityCritical, SeverityHigh}, MaxAttempts: 2}
	if !reflect.DeepEqual(l.Source, want) {
		t.Errorf("Source = %+v, want %+v", l.Source, want)
	}
	if got := l.StepNames(); !reflect.DeepEqual(got, []string{StepNewBranch, StepFixAlerts, StepGates, StepOpenPR, StepWatchCI, StepRetro}) {
		t.Errorf("steps = %v", got)
	}
	if l.LeavesCIToPRLoop() {
		t.Error("only a ticket loop hands its CI on")
	}
	var bodies PromptBodies
	l.ApplyPrompts(&bodies)
	if bodies.SecurityFix != "Prefer an override." || bodies.Fix != "" {
		t.Errorf("fix_alerts supplies the SecurityFix body, got %+v", bodies)
	}

	all, _ := p.LoopNamed("everything")
	if !reflect.DeepEqual(all.Source.Severity, []string{"low", "medium", "high", "critical"}) || all.Source.MaxAttempts != 4 {
		t.Errorf("a loop's own severity and budget are kept, got %+v", all.Source)
	}
}

// The rest of what a loop can get wrong, each named.
func TestLoadProjectRejectsLoopMisconfigurations(t *testing.T) {
	cases := []struct {
		name  string
		loops string
		want  string
	}{
		{"no name", `
[[loops]]
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }]
`, "loops[0].name is required"},
		{"bad name", `
[[loops]]
name = "PR Doctor"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }]
`, `name "PR Doctor" must be lowercase letters, digits and dashes`},
		{"duplicate", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }]

[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }]
`, `loops[1] ("a"): name "a" is already declared`},
		{"no source", `
[[loops]]
name = "a"
steps = [{ use = "implement" }, { use = "review" }]
`, "source.kind is required"},
		{"no steps", `
[[loops]]
name = "a"
source = { kind = "tracker" }
`, "at least one step is required"},
		{"bad duration", `
[[loops]]
name = "a"
poll_interval = "10 minutes"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }]
`, `poll_interval "10 minutes" is not a duration`},
		{"unknown scope", `
[[loops]]
name = "a"
source = { kind = "github_prs", scope = "everything" }
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates" }, { use = "push_existing" }]
`, `unknown scope "everything"`},
		{"label with scope all", `
[[loops]]
name = "a"
source = { kind = "github_prs", scope = "all", label = "x" }
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates" }, { use = "push_existing" }]
`, "source.label has no effect"},
		{"scope on a tracker", `
[[loops]]
name = "a"
source = { kind = "tracker", scope = "all" }
steps = [{ use = "implement" }, { use = "review" }]
`, "source.scope is not a tracker parameter"},
		{"label on a backlog", `
[[loops]]
name = "a"
source = { kind = "backlog", label = "x" }
steps = [{ use = "assess" }, { use = "apply" }]
`, "source.label is not a backlog parameter"},
		{"apply before assess", `
[[loops]]
name = "a"
source = { kind = "backlog" }
steps = [{ use = "apply" }, { use = "assess" }]
`, "apply needs assess before it"},
		{"backlog without assess", `
[[loops]]
name = "a"
source = { kind = "backlog" }
steps = [{ use = "apply" }]
`, "a backlog loop needs a assess step"},
		{"retro on a backlog", `
[[loops]]
name = "a"
source = { kind = "backlog" }
steps = [{ use = "assess" }, { use = "retro" }]
`, "retro does not run on a backlog source"},
		{"ticket step on a backlog", `
[[loops]]
name = "a"
source = { kind = "backlog" }
steps = [{ use = "assess" }, { use = "implement" }]
`, "implement does not run on a backlog source"},
		{"negative count", `
[[loops]]
name = "a"
max_consecutive_failures = -1
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }]
`, "max_consecutive_failures must be positive"},
		{"step from the other source", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }, { use = "gates" }]
`, "gates does not run on a tracker source"},
		{"watch_ci before review on a tracker", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "watch_ci" }, { use = "review" }]
`, "watch_ci needs review before it"},
		{"missing required step", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }]
`, "a tracker loop needs a review step"},
		{"duplicate step", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }, { use = "review" }]
`, "review appears twice"},
		{"retro not last", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "retro" }, { use = "review" }]
`, "retro must be the last step"},
		{"prompt on a step that takes none", `
[[loops]]
name = "a"
source = { kind = "github_prs" }
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates", prompt = "prompts/x.md" }, { use = "push_existing" }, { use = "watch_ci" }]
`, "gates takes no prompt"},
		{"ci_failing without watch_ci", `
[[loops]]
name = "a"
source = { kind = "github_prs", when = ["ci_failing"] }
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates" }, { use = "push_existing" }]
`, "when = [\"ci_failing\"] needs a watch_ci step"},
		{"unknown when", `
[[loops]]
name = "a"
source = { kind = "github_prs", when = ["stale"] }
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates" }, { use = "push_existing" }, { use = "watch_ci" }]
`, `unknown when "stale"`},
		{"tracker takes no label", `
[[loops]]
name = "a"
source = { kind = "tracker", label = "x" }
steps = [{ use = "implement" }, { use = "review" }]
`, "source.label is not a tracker parameter"},
		{"unknown source lists every kind", `
[[loops]]
name = "a"
source = { kind = "jira_board" }
steps = [{ use = "implement" }, { use = "review" }]
`, `use "tracker", "github_prs", "github_security_alerts" or "backlog"`},
		{"security alerts take no label", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts", label = "x" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`, "source.label is not a github_security_alerts parameter"},
		{"security alerts take no scope", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts", scope = "all" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`, "source.scope is not a github_security_alerts parameter"},
		{"security alerts take no when", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts", when = ["conflicting"] }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`, "source.when is not a github_security_alerts parameter"},
		{"unknown severity", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts", severity = ["urgent"] }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`, `unknown severity "urgent": use "critical", "high", "medium" or "low"`},
		{"severity on a PR loop", `
[[loops]]
name = "a"
source = { kind = "github_prs", when = ["conflicting"], severity = ["high"] }
steps = [{ use = "checkout_pr" }, { use = "fix" }, { use = "gates" }, { use = "push_existing" }]
`, "source.severity is not a github_prs parameter"},
		{"negative security budget", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts", max_attempts = -1 }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`, "source.max_attempts must be positive"},
		{"fix_alerts before new_branch", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "fix_alerts" }, { use = "new_branch" }, { use = "gates" }, { use = "open_pr" }]
`, "fix_alerts needs new_branch before it"},
		{"gates before fix_alerts", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "gates" }, { use = "fix_alerts" }, { use = "open_pr" }]
`, "gates needs fix_alerts before it"},
		{"open_pr before gates", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "open_pr" }, { use = "gates" }]
`, "open_pr needs gates before it"},
		{"watch_ci before open_pr", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "watch_ci" }, { use = "open_pr" }]
`, "watch_ci needs open_pr before it"},
		{"security retro not last", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "retro" }, { use = "open_pr" }]
`, "retro must be the last step"},
		{"security loop without open_pr", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }]
`, "a github_security_alerts loop needs a open_pr step"},
		{"a PR loop's fix on a security loop", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "fix" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr" }]
`, "fix does not run on a github_security_alerts source"},
		{"new_branch on a tracker", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "new_branch" }, { use = "implement" }, { use = "review" }]
`, "new_branch does not run on a tracker source"},
		{"open_pr takes no prompt", `
[[loops]]
name = "a"
source = { kind = "github_security_alerts" }
steps = [{ use = "new_branch" }, { use = "fix_alerts" }, { use = "gates" }, { use = "open_pr", prompt = "prompts/x.md" }]
`, "open_pr takes no prompt"},
		{"prompt escapes the directory", `
[[loops]]
name = "a"
source = { kind = "tracker" }
steps = [{ use = "implement", prompt = "../secrets.md" }, { use = "review" }]
`, "must stay inside .agent-harness/"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadProject(loopsCheckout(t, c.loops))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("want an error mentioning %q, got:\n%v", c.want, err)
			}
		})
	}
}

// A loop instance (#89) keeps its own sentinel and its own logs, beside the
// loop's: STOP-<id> stops it alone, and its stream never mixes with another's.
// The default instance keeps the loop's paths unchanged.
func TestALoopInstanceHasItsOwnSentinelAndLogs(t *testing.T) {
	p := Project{}
	b, err := p.LoopInstance("", "b")
	if err != nil {
		t.Fatalf("LoopInstance: %v", err)
	}
	if b.Name != DefaultLoopName || b.Instance != "b" {
		t.Errorf("instance = %+v, want instance b of the ticket loop", b)
	}
	if b.StopFile() != ".agent-harness/STOP-tickets" || b.InstanceStopFile() != ".agent-harness/STOP-b" {
		t.Errorf("sentinels = %q, %q; want the loop's and the instance's own", b.StopFile(), b.InstanceStopFile())
	}
	if b.LogsDir() != ".agent-harness/logs/instances/b" {
		t.Errorf("logs = %q, want .agent-harness/logs/instances/b", b.LogsDir())
	}

	plain, err := p.LoopInstance("", "")
	if err != nil {
		t.Fatalf("LoopInstance: %v", err)
	}
	if plain.InstanceStopFile() != "" || plain.LogsDir() != ".agent-harness/logs" {
		t.Errorf("the default instance moved: sentinel %q, logs %q", plain.InstanceStopFile(), plain.LogsDir())
	}
}

// An instance id names files, so it is held to the loop-name rule, and it may
// not be a loop's name, or STOP-<id> would stop that loop too. Only a ticket
// loop runs as several instances: the PR loop's reaper assumes it is alone.
func TestLoopInstanceRefusesWhatWouldCollide(t *testing.T) {
	p := Project{}
	for _, tc := range []struct{ loop, instance, want string }{
		{"", "B/../x", "lowercase"},
		{"", "pr-doctor", "names a loop"},
		{"", "tickets", "names a loop"},
		{"", "default", "watch --all"},
		{"pr-doctor", "b", "ticket loop"},
		{"nope", "b", "no loop named"},
	} {
		_, err := p.LoopInstance(tc.loop, tc.instance)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoopInstance(%q, %q) = %v, want an error mentioning %q", tc.loop, tc.instance, err, tc.want)
		}
	}
}

// Every daemon `agent-harness start` launches has its own pidfile, so the viewer
// can probe the one it follows (#106): the default ticket loop keeps loop.pid,
// another loop has loop-<name>.pid, and an instance has loop-<id>.pid whichever
// loop it runs. Ids may not be loop names, so the two never share a file.
func TestEachDaemonHasItsOwnPidFile(t *testing.T) {
	for _, tc := range []struct {
		loop Loop
		want string
	}{
		{Loop{Name: DefaultLoopName}, ".agent-harness/loop.pid"},
		{Loop{}, ".agent-harness/loop.pid"},
		{Loop{Name: PRDoctorLoopName}, ".agent-harness/loop-pr-doctor.pid"},
		{Loop{Name: DefaultLoopName, Instance: "b"}, ".agent-harness/loop-b.pid"},
		{Loop{Name: "triage", Instance: "b"}, ".agent-harness/loop-b.pid"},
	} {
		if got := tc.loop.PidFile(); got != tc.want {
			t.Errorf("%+v.PidFile() = %q, want %q", tc.loop, got, tc.want)
		}
	}
}
