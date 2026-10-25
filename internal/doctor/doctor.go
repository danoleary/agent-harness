// Package doctor checks a whole harness setup in one pass and reports every
// problem it finds, each with the fix — `agent-harness doctor`, in the manner of
// `flutter doctor`. A run of any Stage stops at its first setup error, so a
// newcomer with several (no Docker, an old git, a fine-grained GH_TOKEN, a missing
// prompt body) used to discover them one run at a time.
//
// Every check is read-only. The credential checks make one live, read-only API
// call each, because "set" and "valid" are different questions and only the
// second one matters; --offline skips every network call and says which lines it
// therefore did not verify (ADR-0020).
//
// The probes — the command runner, the HTTP client, the API base URLs — are
// injected through Probes, so the whole checklist is exercised against fakes.
package doctor

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/sandbox"
)

// Status is one check's verdict.
type Status int

const (
	// OK — the check passed.
	OK Status = iota
	// Warn — the setup works, but something about it will surprise the operator.
	Warn
	// Fail — a run would fail, or misbehave, because of this. Any Fail makes
	// doctor exit non-zero.
	Fail
	// Skip — the check could not run: --offline, or a prerequisite failed. The
	// detail says which, so a skip never reads as a pass.
	Skip
)

func (s Status) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "skip"
	}
}

// mark is the checklist glyph for a status.
func (s Status) mark() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	case Fail:
		return "✗"
	default:
		return "-"
	}
}

// Result is one line of the checklist.
type Result struct {
	// Name is what was checked ("docker", "GH_TOKEN", "tracker", …).
	Name   string
	Status Status
	// Detail says what was found. It may span several lines.
	Detail string
	// Fix is the action that clears a Fail or Warn; empty on OK and Skip.
	Fix string
}

// Probes is everything doctor reads the world through.
type Probes struct {
	// Getenv reads the environment, after LoadEnv has filled it from the .env.
	Getenv config.Getenv
	// WorkingDir is where the harness was started; the .env search starts there.
	WorkingDir string
	// LoadEnv loads the resolved .env into the environment Getenv reads, exactly
	// as a Stage's config load does. nil loads nothing (the tests set Getenv
	// directly).
	LoadEnv func(path string)
	// Run runs a bounded host command and returns its combined output.
	Run func(name string, args ...string) ([]byte, error)
	// HTTP makes the live credential checks.
	HTTP *http.Client
	// GitHubAPI, AnthropicAPI and LinearAPI are the API endpoints. The Jira site
	// is the operator's own, so it comes from JIRA_BASE_URL instead.
	GitHubAPI    string
	AnthropicAPI string
	LinearAPI    string
	// Offline skips every network call: the credential, tracker and image-pull
	// checks report Skip instead.
	Offline bool
}

// httpTimeout bounds each live API call, so a dead network fails a line instead
// of hanging the whole checklist.
const httpTimeout = 20 * time.Second

// Live is the production Probes: the real environment, the real tools, the real
// APIs.
func Live(offline bool) Probes {
	wd, _ := os.Getwd()
	return Probes{
		Getenv:       os.Getenv,
		WorkingDir:   wd,
		LoadEnv:      config.LoadDotEnv,
		Run:          sandbox.ProbeRunner,
		HTTP:         &http.Client{Timeout: httpTimeout},
		GitHubAPI:    "https://api.github.com",
		AnthropicAPI: "https://api.anthropic.com",
		LinearAPI:    "https://api.linear.app/graphql",
		Offline:      offline,
	}
}

// Run performs every check, in checklist order, and returns them all. It never
// stops early: reporting every problem at once is the point.
func Run(p Probes) []Result {
	env := checkEnvFile(p)
	tools := []Result{checkDocker(p), checkGit(p), checkGH(p)}
	daemonUp := tools[0].Status == OK

	results := append(tools, env)
	results = append(results, checkProjectPath(p))
	results = append(results, checkClaude(p), checkGitHubToken(p))

	proj := loadProject(p)
	results = append(results, proj.configResult, proj.promptsResult)
	results = append(results, checkTracker(p, proj), checkImage(p, proj, daemonUp))
	return results
}

// Failed reports whether any check failed — doctor's exit status, so it works in
// a script.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

// Render writes the checklist: one line per check, its fix indented beneath it,
// then a tally.
func Render(w io.Writer, results []Result) {
	var problems, warnings int
	for _, r := range results {
		lines := strings.Split(strings.TrimRight(r.Detail, "\n"), "\n")
		fmt.Fprintf(w, "[%s] %s: %s\n", r.Status.mark(), r.Name, lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(w, "      %s\n", l)
		}
		if r.Fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", r.Fix)
		}
		switch r.Status {
		case Fail:
			problems++
		case Warn:
			warnings++
		}
	}
	if problems == 0 && warnings == 0 {
		fmt.Fprintln(w, "\nNo problems found.")
		return
	}
	fmt.Fprintf(w, "\n%s, %s.\n", plural(problems, "problem"), plural(warnings, "warning"))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// firstLine is the informative line of a tool's output: docker's own reason when
// it gives one, else the first non-blank line.
func firstLine(out []byte, err error) string {
	if reason := sandbox.DockerErrorReason(string(out)); reason != "" {
		return reason
	}
	if err != nil {
		return err.Error()
	}
	return ""
}
