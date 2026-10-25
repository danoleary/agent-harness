package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A loop is a Source and a Pipeline run by the shared daemon (ADR-0019). A
// Consumer declares its loops as [[loops]] in config.toml: a Source kind with its
// parameters, an ordered list of Steps, and per-loop limits. Both sets are fixed
// and implemented in Go, and every Step owns its ground-truth check, so config
// chooses, orders and parameterises Steps but cannot script one: there is no step
// that runs a shell command, or an agent with no check after it.

// The Source kinds a loop can poll.
const (
	// SourceTracker is the Tracker's ready queue: the ticket loop.
	SourceTracker = "tracker"
	// SourceGitHubPRs is the open PRs that conflict with main or have red CI: the
	// PR loop.
	SourceGitHubPRs = "github_prs"
)

// The Steps a loop's Pipeline is made of.
const (
	// StepImplement is the Implementation Stage: a /tdd session into a fresh
	// worktree, checked for a committed handoff.
	StepImplement = "implement"
	// StepReview is the Review Stage: a cold review, the Gates, and the Landing.
	StepReview = "review"
	// StepRetro is the Retrospective: it files the run's Findings.
	StepRetro = "retro"
	// StepCheckoutPR checks the PR's head branch out at the polled head and runs
	// post_create.
	StepCheckoutPR = "checkout_pr"
	// StepFix runs the RebaseFix session over a conflicting PR, checked by the
	// rebase/merge ground truth.
	StepFix = "fix"
	// StepGates re-runs every Gate over the fixed tree.
	StepGates = "gates"
	// StepPushExisting pushes the fixed branch to its existing PR.
	StepPushExisting = "push_existing"
	// StepWatchCI watches CI to a verdict, running a CIFix session over red checks
	// whose committed fix must pass every Gate before it is pushed. On a ticket
	// loop the review stage runs it over the PR it just opened.
	StepWatchCI = "watch_ci"
)

// What a github_prs Source yields ("when").
const (
	WhenConflicting = "conflicting"
	WhenCIFailing   = "ci_failing"
)

// The built-in loops' names. DefaultLoopName is what a bare `agent-harness loop`
// runs.
const (
	DefaultLoopName  = "tickets"
	PRDoctorLoopName = "pr-doctor"
)

// LoopConfig is one [[loops]] entry as written.
type LoopConfig struct {
	Name                   string       `toml:"name"`
	PollInterval           string       `toml:"poll_interval"`
	CapBackoff             string       `toml:"cap_backoff"`
	ClaimTTL               string       `toml:"claim_ttl"`
	MaxRuntime             string       `toml:"max_runtime"`
	MaxConsecutiveFailures int          `toml:"max_consecutive_failures"`
	MaxItems               int          `toml:"max_items"`
	Source                 SourceConfig `toml:"source"`
	Steps                  []StepConfig `toml:"steps"`
}

// SourceConfig is a loop's `source = { kind = …, … }` as written.
type SourceConfig struct {
	Kind        string   `toml:"kind"`
	Label       string   `toml:"label"`
	When        []string `toml:"when"`
	MaxAttempts int      `toml:"max_attempts"`
}

// StepConfig is one `{ use = …, prompt = … }` in a loop's steps.
type StepConfig struct {
	Use    string `toml:"use"`
	Prompt string `toml:"prompt"`
}

// Loop is one loop, resolved and validated.
type Loop struct {
	Name string
	// Instance names one of several daemons running this loop against one
	// Consumer at once (#89), each with its own sentinel and logs. Empty is the
	// default instance, whose paths are the loop's own.
	Instance string
	// BuiltIn marks a loop the harness defines when no [[loops]] entry of that name
	// replaces it.
	BuiltIn bool
	Source  LoopSource
	Steps   []Step
	Limits  LoopLimits
}

// LoopSource is a loop's Source kind and its parameters. A zero parameter means
// the env knob's value (PR_LOOP_LABEL, PR_LOOP_MAX_ATTEMPTS).
type LoopSource struct {
	Kind string
	// Label is the github_prs opt-in label.
	Label string
	// When is what a github_prs Source yields: conflicting PRs, PRs with red CI.
	When []string
	// MaxAttempts is the github_prs per-PR attempt budget.
	MaxAttempts int
}

// Yields reports whether a github_prs Source yields PRs in this state.
func (s LoopSource) Yields(when string) bool {
	for _, w := range s.When {
		if w == when {
			return true
		}
	}
	return false
}

// Step is one Step of a loop's Pipeline. Prompt is the body read from PromptPath
// (relative to .agent-harness/), which replaces the Step's default Project body;
// empty keeps the default.
type Step struct {
	Use        string
	PromptPath string
	Prompt     string
}

// LoopLimits are a loop's own daemon limits. A zero field means the LOOP_* env
// knob's value.
type LoopLimits struct {
	PollInterval           time.Duration
	CapBackoff             time.Duration
	ClaimTTL               time.Duration
	MaxRuntime             time.Duration
	MaxConsecutiveFailures int
	MaxItems               int
}

// Has reports whether the loop runs the step.
func (l Loop) Has(use string) bool {
	for _, s := range l.Steps {
		if s.Use == use {
			return true
		}
	}
	return false
}

// LeavesCIToPRLoop reports whether a ticket loop's review stage returns as soon
// as the PR is open, leaving red CI to a PR loop (#110): it is a ticket loop that
// leaves out watch_ci. The built-in ticket loop keeps it, so a Consumer that runs
// no PR loop still has its red CI fixed.
func (l Loop) LeavesCIToPRLoop() bool {
	return l.Source.Kind == SourceTracker && !l.Has(StepWatchCI)
}

// Prompt is the body the loop supplies for a step, or "" for the default.
func (l Loop) Prompt(use string) string {
	for _, s := range l.Steps {
		if s.Use == use {
			return s.Prompt
		}
	}
	return ""
}

// StepNames is the loop's steps in order.
func (l Loop) StepNames() []string {
	out := make([]string, 0, len(l.Steps))
	for _, s := range l.Steps {
		out = append(out, s.Use)
	}
	return out
}

// StopFile is the loop's own STOP sentinel, relative to the checkout. The plain
// STOP sentinel stops every loop as well.
func (l Loop) StopFile() string { return ProjectDirName + "/STOP-" + l.Name }

// InstanceStopFile is a named instance's own STOP sentinel (STOP-<instance>),
// relative to the checkout, or "" for the default instance. The loop's sentinel
// and the plain STOP stop the instance as well.
func (l Loop) InstanceStopFile() string {
	if l.Instance == "" {
		return ""
	}
	return ProjectDirName + "/STOP-" + l.Instance
}

// PidFile is where `agent-harness start` records this daemon's pid, relative to
// the checkout: loop-<id>.pid for a named instance, loop.pid for the default
// ticket loop, and loop-<name>.pid for any other loop (#106). An id may not name
// a loop, so no two daemons share one. scripts/loop-start.sh writes the same
// names, and the viewer probes them for liveness.
func (l Loop) PidFile() string {
	switch {
	case l.Instance != "":
		return ProjectDirName + "/loop-" + l.Instance + ".pid"
	case l.Name == "" || l.Name == DefaultLoopName:
		return ProjectDirName + "/loop.pid"
	default:
		return ProjectDirName + "/loop-" + l.Name + ".pid"
	}
}

// DefaultInstanceLabel is how `agent-harness watch --all` labels the instance
// started with no id, so no named instance may take it.
const DefaultInstanceLabel = "default"

// InstancesLogsDir is where named instances keep their logs, one dir each,
// relative to the checkout. `agent-harness watch --all` reads it.
const InstancesLogsDir = ProjectDirName + "/logs/instances"

// LogsDir is where the loop keeps its per-item log dirs and the loop.jsonl stream
// the viewer follows, relative to the checkout. The default ticket loop keeps the
// logs/ it always had, so `agent-harness watch` finds it unchanged; every other
// loop has its own dir beside it, so loops can run side by side. A named instance
// has its own dir under logs/instances/, so two instances' streams never mix.
func (l Loop) LogsDir() string {
	if l.Instance != "" {
		return filepath.Join(InstancesLogsDir, l.Instance)
	}
	if l.Name == DefaultLoopName {
		return filepath.Join(ProjectDirName, "logs")
	}
	return filepath.Join(ProjectDirName, "logs", "loops", l.Name)
}

// ApplyPrompts replaces the Project bodies the loop's steps supply.
func (l Loop) ApplyPrompts(p *PromptBodies) {
	for _, s := range l.Steps {
		if s.Prompt == "" {
			continue
		}
		switch s.Use {
		case StepImplement:
			p.Implement = s.Prompt
		case StepReview:
			p.Review = s.Prompt
		case StepRetro:
			p.Retro = s.Prompt
		case StepFix:
			p.Fix = s.Prompt
		}
	}
}

// BuiltInLoops are the loops the harness defines: the ticket loop, and the PR
// loop that was `loop --prs` before loops had names.
func BuiltInLoops() []Loop {
	steps := func(uses ...string) []Step {
		out := make([]Step, 0, len(uses))
		for _, u := range uses {
			out = append(out, Step{Use: u})
		}
		return out
	}
	return []Loop{
		{
			Name: DefaultLoopName, BuiltIn: true,
			Source: LoopSource{Kind: SourceTracker},
			Steps:  steps(StepImplement, StepReview, StepWatchCI, StepRetro),
		},
		{
			Name: PRDoctorLoopName, BuiltIn: true,
			Source: LoopSource{Kind: SourceGitHubPRs, When: []string{WhenConflicting, WhenCIFailing}},
			Steps:  steps(StepCheckoutPR, StepFix, StepGates, StepPushExisting, StepWatchCI, StepRetro),
		},
	}
}

// AllLoops is every loop `agent-harness loop <name>` can run: the built-ins,
// each replaced by a declared loop of the same name, then the other declared
// loops in the order they are written.
func (p Project) AllLoops() []Loop {
	declared := make(map[string]Loop, len(p.Loops))
	for _, l := range p.Loops {
		declared[l.Name] = l
	}
	var out []Loop
	for _, b := range BuiltInLoops() {
		if l, ok := declared[b.Name]; ok {
			out = append(out, l)
			delete(declared, b.Name)
			continue
		}
		out = append(out, b)
	}
	for _, l := range p.Loops {
		if _, ok := declared[l.Name]; ok {
			out = append(out, l)
		}
	}
	return out
}

// LoopNamed finds a loop by name; "" names the default ticket loop.
func (p Project) LoopNamed(name string) (Loop, bool) {
	if name == "" {
		name = DefaultLoopName
	}
	for _, l := range p.AllLoops() {
		if l.Name == name {
			return l, true
		}
	}
	return Loop{}, false
}

// LoopInstance is the named loop run as instance id (#89); an empty id is the
// default instance. An id names the instance's sentinel, pidfile and logs, so it
// follows the loop-name rule and may not be a loop's name: STOP-<id> must stop
// that one instance and nothing else. Only a ticket loop runs as several
// instances; the PR loop's reaper assumes it is the only PR loop on the checkout.
func (p Project) LoopInstance(name, id string) (Loop, error) {
	l, ok := p.LoopNamed(name)
	if !ok {
		return Loop{}, fmt.Errorf("no loop named %q in this project", name)
	}
	if id == "" {
		return l, nil
	}
	if !loopNameRE.MatchString(id) {
		return Loop{}, fmt.Errorf("instance %q must be lowercase letters, digits and dashes: it names the instance's STOP sentinel, pidfile and log dir", id)
	}
	if _, clash := p.LoopNamed(id); clash {
		return Loop{}, fmt.Errorf("instance %q names a loop, so STOP-%s would stop that loop too; pick another id", id, id)
	}
	if id == DefaultInstanceLabel {
		return Loop{}, fmt.Errorf("instance %q is the name `agent-harness watch --all` gives the instance started with no id; pick another id", id)
	}
	if l.Source.Kind != SourceTracker {
		return Loop{}, fmt.Errorf("loop %q cannot run as instance %q: only a ticket loop (source %q) runs as several instances", l.Name, id, SourceTracker)
	}
	l.Instance = id
	return l, nil
}

// stepSpec is what the loader knows about one Step: the Sources it runs on, the
// step that must come before it on each (and why), and whether it takes a prompt
// body.
type stepSpec struct {
	// on maps each Source the step runs on to what it needs there; nil runs on
	// every Source with nothing before it.
	on     map[string]prereq
	prompt bool
	// last marks a step nothing may follow.
	last bool
}

// prereq is the step that must come before another on one Source, and why.
type prereq struct {
	needs string
	why   string
}

var stepSpecs = map[string]stepSpec{
	StepImplement:    {on: map[string]prereq{SourceTracker: {}}, prompt: true},
	StepReview:       {on: map[string]prereq{SourceTracker: {StepImplement, "it reviews the branch implement committed"}}, prompt: true},
	StepCheckoutPR:   {on: map[string]prereq{SourceGitHubPRs: {}}},
	StepFix:          {on: map[string]prereq{SourceGitHubPRs: {StepCheckoutPR, "it fixes the worktree checkout_pr makes"}}, prompt: true},
	StepGates:        {on: map[string]prereq{SourceGitHubPRs: {StepFix, "it checks the tree fix committed"}}},
	StepPushExisting: {on: map[string]prereq{SourceGitHubPRs: {StepGates, "it pushes only a committed, gate-green tree"}}},
	// On a ticket loop, watch_ci keeps the review stage on its PR's CI to a verdict;
	// a ticket loop without it leaves red CI to a PR loop (#110).
	StepWatchCI: {on: map[string]prereq{
		SourceTracker:   {StepReview, "it watches the CI of the PR review opens"},
		SourceGitHubPRs: {StepPushExisting, "it watches the CI of a pushed head"},
	}},
	StepRetro: {prompt: true, last: true},
}

// stepOrder lists the steps in the order the loader names them in an error.
var stepOrder = []string{StepImplement, StepReview, StepCheckoutPR, StepFix, StepGates, StepPushExisting, StepWatchCI, StepRetro}

// requiredSteps are the steps without which a Source's loop would do nothing
// worth a sandbox: a ticket loop that never lands, a PR loop that never pushes.
var requiredSteps = map[string][]string{
	SourceTracker:   {StepImplement, StepReview},
	SourceGitHubPRs: {StepCheckoutPR, StepFix, StepGates, StepPushExisting},
}

var loopNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// IsLoopName reports whether s follows the rule every loop name and instance id
// does: lowercase letters, digits and dashes, not starting with a dash. Such a
// name is safe to put in a file name.
func IsLoopName(s string) bool { return loopNameRE.MatchString(s) }

// resolveLoops validates every [[loops]] entry against the fixed Source and Step
// sets, reads the prompt bodies they name, and returns the resolved loops with
// every problem found, each at its file:line:column when the loader could place
// it. dir is the .agent-harness/ directory prompt paths are relative to.
func resolveLoops(loops []LoopConfig, dir string, at positions) ([]Loop, []string) {
	var out []Loop
	var problems []string
	seen := map[string]bool{}
	for i, lc := range loops {
		path := fmt.Sprintf("loops[%d]", i)
		label := path
		if lc.Name != "" {
			label = fmt.Sprintf("%s (%q)", path, lc.Name)
		}
		// add reports a problem with the loop at key, labelled by the loop and, for
		// a step, by the step's index.
		add := func(key, format string, a ...any) {
			where := label
			if step := strings.TrimPrefix(key, path+"."); strings.HasPrefix(step, "steps[") {
				where += " " + step
			}
			problems = append(problems, at.prefix(key, path)+where+": "+fmt.Sprintf(format, a...))
		}

		switch {
		case lc.Name == "":
			problems = append(problems, at.prefix(path, path)+path+".name is required: it is what `agent-harness loop <name>` runs")
		case !loopNameRE.MatchString(lc.Name):
			add(path+".name", "name %q must be lowercase letters, digits and dashes: it names the loop's STOP sentinel and log dir", lc.Name)
		case seen[lc.Name]:
			add(path+".name", "name %q is already declared", lc.Name)
		}
		seen[lc.Name] = true

		l := Loop{Name: lc.Name}
		l.Limits = resolveLimits(lc, path, add)

		l.Source = LoopSource{Kind: lc.Source.Kind, Label: lc.Source.Label, When: lc.Source.When, MaxAttempts: lc.Source.MaxAttempts}
		src := path + ".source"
		switch lc.Source.Kind {
		case "":
			add(path, "source.kind is required: %q or %q", SourceTracker, SourceGitHubPRs)
		case SourceTracker:
			if lc.Source.Label != "" {
				add(src+".label", "source.label is not a tracker parameter: the ticket queue is set under [tracker]")
			}
			if len(lc.Source.When) > 0 {
				add(src+".when", "source.when is not a tracker parameter")
			}
			if lc.Source.MaxAttempts != 0 {
				add(src+".max_attempts", "source.max_attempts is not a tracker parameter")
			}
		case SourceGitHubPRs:
			if len(l.Source.When) == 0 {
				l.Source.When = []string{WhenConflicting, WhenCIFailing}
			}
			for _, w := range l.Source.When {
				if w != WhenConflicting && w != WhenCIFailing {
					add(src+".when", "unknown when %q: use %q or %q", w, WhenConflicting, WhenCIFailing)
				}
			}
			if lc.Source.MaxAttempts < 0 {
				add(src+".max_attempts", "source.max_attempts must be positive, got %d", lc.Source.MaxAttempts)
			}
		default:
			add(src+".kind", "unknown source kind %q: use %q or %q", lc.Source.Kind, SourceTracker, SourceGitHubPRs)
		}

		l.Steps = resolveSteps(lc, l.Source, path, dir, add)
		out = append(out, l)
	}
	return out, problems
}

// resolveLimits parses a loop's own limits.
func resolveLimits(lc LoopConfig, path string, add func(key, format string, a ...any)) LoopLimits {
	duration := func(key, raw string) time.Duration {
		if raw == "" {
			return 0
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			add(path+"."+key, "%s %q is not a duration: write a positive Go duration such as \"10m\" or \"1h30m\"", key, raw)
			return 0
		}
		return d
	}
	count := func(key string, n int) int {
		if n < 0 {
			add(path+"."+key, "%s must be positive, got %d", key, n)
			return 0
		}
		return n
	}
	return LoopLimits{
		PollInterval:           duration("poll_interval", lc.PollInterval),
		CapBackoff:             duration("cap_backoff", lc.CapBackoff),
		ClaimTTL:               duration("claim_ttl", lc.ClaimTTL),
		MaxRuntime:             duration("max_runtime", lc.MaxRuntime),
		MaxConsecutiveFailures: count("max_consecutive_failures", lc.MaxConsecutiveFailures),
		MaxItems:               count("max_items", lc.MaxItems),
	}
}

// resolveSteps checks a loop's steps fit together and fit its Source, and reads
// the prompt bodies they name.
func resolveSteps(lc LoopConfig, src LoopSource, path, dir string, add func(key, format string, a ...any)) []Step {
	if len(lc.Steps) == 0 {
		add(path, "at least one step is required")
		return nil
	}
	known := src.Kind == SourceTracker || src.Kind == SourceGitHubPRs
	var steps []Step
	before := map[string]bool{}
	for j, sc := range lc.Steps {
		key := fmt.Sprintf("%s.steps[%d]", path, j)
		stepAdd := func(format string, a ...any) { add(key, format, a...) }
		spec, ok := stepSpecs[sc.Use]
		switch {
		case sc.Use == "":
			stepAdd("use is required: one of %s", strings.Join(stepOrder, ", "))
			continue
		case !ok:
			stepAdd("unknown step %q: use one of %s", sc.Use, strings.Join(stepOrder, ", "))
			continue
		}
		step := Step{Use: sc.Use, PromptPath: sc.Prompt}
		if sc.Prompt != "" {
			if !spec.prompt {
				stepAdd("%s takes no prompt: it runs no agent session", sc.Use)
			} else if body, problem := readStepPrompt(dir, sc.Prompt); problem != "" {
				stepAdd("%s", problem)
			} else {
				step.Prompt = body
			}
		}
		if known {
			pre, runsHere := spec.on[src.Kind]
			switch {
			case spec.on != nil && !runsHere:
				stepAdd("%s does not run on a %s source", sc.Use, src.Kind)
			case before[sc.Use]:
				stepAdd("%s appears twice", sc.Use)
			case pre.needs != "" && !before[pre.needs]:
				stepAdd("%s needs %s before it: %s", sc.Use, pre.needs, pre.why)
			case spec.last && j != len(lc.Steps)-1:
				stepAdd("%s must be the last step: it mines the transcripts every step before it left", sc.Use)
			case sc.Use == StepRetro && j == 0:
				stepAdd("retro needs a step before it: it mines that step's transcripts")
			}
		}
		before[sc.Use] = true
		steps = append(steps, step)
	}
	if !known {
		return steps
	}
	for _, req := range requiredSteps[src.Kind] {
		if !before[req] {
			add(path+".steps", "a %s loop needs a %s step", src.Kind, req)
		}
	}
	if src.Kind == SourceGitHubPRs && src.Yields(WhenCIFailing) && !before[StepWatchCI] {
		add(path+".source.when", "when = [%q] needs a watch_ci step: a red check is fixed inside the CI watch", WhenCIFailing)
	}
	return steps
}

// readStepPrompt reads a step's prompt body from dir, or says why it cannot.
func readStepPrompt(dir, rel string) (string, string) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(rel) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Sprintf("prompt %s must stay inside %s/: write it relative to that directory, e.g. prompts/fix-pr.md", rel, ProjectDirName)
	}
	full := filepath.Join(dir, clean)
	raw, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Sprintf("prompt file %s (%s) cannot be read: %v", rel, full, err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", fmt.Sprintf("prompt file %s (%s) is blank", rel, full)
	}
	return string(raw), ""
}
