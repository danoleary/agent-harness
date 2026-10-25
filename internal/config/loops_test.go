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

// With no [[loops]], the two built-in loops are what `loop` can run: the ticket
// loop, which a bare `agent-harness loop` runs as it always has, and pr-doctor.
func TestNoLoopsDeclaredLeavesTheBuiltIns(t *testing.T) {
	pc, err := LoadProject(loopsCheckout(t, ""))
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	p := Project{Loops: pc.ResolvedLoops}
	got := p.AllLoops()
	if len(got) != 2 || got[0].Name != DefaultLoopName || got[1].Name != PRDoctorLoopName {
		t.Fatalf("AllLoops = %+v, want the built-in tickets and pr-doctor loops", got)
	}
	tickets, ok := p.LoopNamed("")
	if !ok || tickets.Name != DefaultLoopName || tickets.Source.Kind != SourceTracker {
		t.Errorf("the unnamed loop is the ticket loop, got %+v", tickets)
	}
	if steps := tickets.StepNames(); !reflect.DeepEqual(steps, []string{StepImplement, StepReview, StepRetro}) {
		t.Errorf("ticket loop steps = %v", steps)
	}
	pr, _ := p.LoopNamed(PRDoctorLoopName)
	if pr.Source.Kind != SourceGitHubPRs || !reflect.DeepEqual(pr.StepNames(), []string{StepCheckoutPR, StepFix, StepGates, StepPushExisting, StepWatchCI, StepRetro}) {
		t.Errorf("pr-doctor = %+v", pr)
	}
	if _, ok := p.LoopNamed("nope"); ok {
		t.Error("an undeclared name is not a loop")
	}
}

// builtInLoopsTOML writes the two built-in loops out as [[loops]] entries.
const builtInLoopsTOML = `
[[loops]]
name = "tickets"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }, { use = "retro" }]

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
	if len(all) != 2 {
		t.Fatalf("AllLoops = %+v, want tickets and the built-in pr-doctor", all)
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
steps = [{ use = "implement" }, { use = "review" }, { use = "watch_ci" }]
`, "watch_ci does not run on a tracker source"},
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
		{"pr-doctor", "b", "ticket loop"},
		{"nope", "b", "no loop named"},
	} {
		_, err := p.LoopInstance(tc.loop, tc.instance)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("LoopInstance(%q, %q) = %v, want an error mentioning %q", tc.loop, tc.instance, err, tc.want)
		}
	}
}
