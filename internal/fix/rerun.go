package fix

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/danoleary/agent-harness/internal/session"
)

// infraSignatures are the log lines that say a gate died of its environment, not
// of the diff, each with the cause the narration names (#167): the test browser's
// connection closing under a test or never opening, a story timing out, Vite
// re-optimising its dependencies and reloading the page mid-run (#312), a process
// killed for memory, a toolchain or package download failing on the network.
// They are built in, not Consumer config. Matched case-insensitively.
//
// A Vite reload is matched only on Vite's own re-optimise and reload lines, never
// on a generic dev-server error such as a pre-transform error or a failed import
// of a test file: a story whose import cannot be resolved prints those too, and
// that red is the diff's (#427). A failed dynamic import counts only for a chunk
// of the optimised deps, reloadImportRE, and only when no transform failure,
// transformFailureRE, explains it (#450).
var infraSignatures = []struct{ cause, needle string }{
	{"the test browser's connection closed", "browser connection was closed"},
	{"the test browser's connection closed", "rpc is closed"},
	{"the test browser's connection closed", "page closed unexpectedly"},
	{"the test browser's connection closed", "browser has disconnected"},
	{"the test browser's connection closed", "target page, context or browser has been closed"},
	{"no test browser session", "failed to connect to the browser session"},
	{"a test timeout", "test timed out in"},
	{viteReload, "optimized dependencies changed"},
	{viteReload, "new dependencies optimized"},
	{viteReload, "vite unexpectedly reloaded a test"},
	{"an out-of-memory kill", "cannot allocate memory"},
	{"an out-of-memory kill", "out of memory"},
	{"a SIGKILL", "sigkill"},
	{"a SIGKILL", "signal: killed"},
	{"a download failure", "error when performing the request"},
	{"a download failure", "econnreset"},
	{"a download failure", "etimedout"},
	{"a download failure", "eai_again"},
	{"a download failure", "socket hang up"},
	{"a download failure", "temporary failure in name resolution"},
	{"a download failure", "tls handshake timeout"},
}

// bootstrapFailure is the cause rerunReason names a failed Corepack download with.
const bootstrapFailure = "Corepack failing to download the package manager"

// corepackNotice is the line Corepack prints before it downloads the package
// manager a project pins, whether or not it prompts. Over lower-cased text.
const corepackNotice = "corepack is about to download"

// bootstrapErrorRE is a fetch failing under that download: any undici error code,
// Node's HTTP/2 stream errors, or a fetch that failed (#470). Over lower-cased text.
var bootstrapErrorRE = regexp.MustCompile(`und_err_[a-z_]+|err_http2_[a-z_]+|fetch failed|error when performing the request|econnreset|etimedout|eai_again|socket hang up`)

// projectStartRE is the header pnpm prints when it runs one of the project's
// scripts: past it, the project's tool is running. Over lower-cased text.
var projectStartRE = regexp.MustCompile(`(?m)^> \S`)

// bootstrapFailed reports whether text, a lower-cased gate log, is a gate that
// never reached the project's tool: Corepack set out to download the package
// manager and a fetch error followed before the project's first script header
// (#470). The download is the harness's toolchain bootstrap, not the tree, so an
// undici error in the project's own tests, after pnpm started them, is not one.
func bootstrapFailed(text string) bool {
	i := strings.LastIndex(text, corepackNotice)
	if i < 0 {
		return false
	}
	after := text[i:]
	if loc := projectStartRE.FindStringIndex(after); loc != nil {
		after = after[:loc[0]]
	}
	return bootstrapErrorRE.MatchString(after)
}

// viteReload is the cause infraSignatures name a Vite reload with.
const viteReload = "a Vite reload"

// reloadImportRE is a failed dynamic import of a chunk Vite optimised, which its
// re-optimisation invalidates mid-run (#312): under Vite's own cache dir
// (/.vite/deps/), Vitest's (/.vite/vitest/<hash>/deps/) or Storybook's
// (/sb-vite/deps/). Over lower-cased text.
var reloadImportRE = regexp.MustCompile(`failed to fetch dynamically imported module: \S*/(?:\.vite|sb-vite)/(?:\S*/)?deps/`)

// transformFailureRE is Vite failing to resolve or transform a module (#450): a
// bare import it cannot resolve, even one inside a pre-bundled dependency, fails
// the browser's import of the optimised-deps chunk that holds it too, so that
// failed import is the transform failure's symptom, not a re-optimisation. Not a
// bare "Pre-transform error": Vite prints one for a stale optimised-deps chunk
// mid-re-optimisation too. Over lower-cased text.
var transformFailureRE = regexp.MustCompile(`failed to resolve import|plugin: vite:import-analysis`)

// notInfraRE is the line a Consumer's gate prints to say its red is the diff's,
// whatever else its log holds: no signature re-runs it (#427). Over lower-cased
// text, as a whole line.
var notInfraRE = regexp.MustCompile(`(?m)^\s*gate-classification:\s*not-infra\s*$`)

// noReoptimiseRE is a gate saying in prose that its red is no dependency
// re-optimisation, which rules out a Vite reload (#427). Over lower-cased text.
var noReoptimiseRE = regexp.MustCompile(`not a (?:vite )?dependency re-?optimi[sz]ation`)

// rerunReason says why a gate that ended in out, having written log, deserves one
// re-run before it counts as red, or "" when its red is final. A re-run costs one
// more gate run, and only a second red counts, so the signals lean towards
// re-running: a red the diff caused is red again.
//
// fixed, when not nil, is the files the fix being gated changed: a red whose
// failures name only other files is re-run too.
func rerunReason(out session.Outcome, log string, fixed []string) string {
	switch {
	case out.ExitCode == 0:
		return ""
	case out.Retryable():
		// The OOM-retry schedule already re-ran this outcome until it ran out.
		return ""
	case out.CapKilled:
		return "the gate cap"
	}
	text := strings.ToLower(log + "\n" + out.StderrTail)
	if notInfraRE.MatchString(text) {
		return ""
	}
	if bootstrapFailed(text) {
		return bootstrapFailure
	}
	reloads := !noReoptimiseRE.MatchString(text)
	for _, s := range infraSignatures {
		if s.cause == viteReload && !reloads {
			continue
		}
		if strings.Contains(text, s.needle) {
			return s.cause
		}
	}
	if reloads && !transformFailureRE.MatchString(text) && reloadImportRE.MatchString(text) {
		return viteReload
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "killed" {
			return "a SIGKILL"
		}
	}
	if fixed != nil && failsOnlyOutside(log, fixed) {
		return "failures only in files the fix did not touch"
	}
	return ""
}

// failurePathRE is a path with a directory and an extension, as a test runner names
// the file a failure is in.
var failurePathRE = regexp.MustCompile(`(?:[\w@.+-]+/)+[\w@.+-]+\.[A-Za-z0-9]+`)

// failsOnlyOutside reports whether log's failure lines name at least one file, and
// none of them is in fixed. A named path matches a changed file when it ends with
// it, since a runner may print it absolute, inside the container.
func failsOnlyOutside(log string, fixed []string) bool {
	named := false
	for _, line := range strings.Split(log, "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "fail") && !strings.Contains(lower, "error") {
			continue
		}
		for _, p := range failurePathRE.FindAllString(line, -1) {
			named = true
			for _, f := range fixed {
				if p == f || strings.HasSuffix(p, "/"+f) {
					return false
				}
			}
		}
	}
	return named
}

// gateLog is a gate's step log, a name under the run's log dir; "" when it cannot
// be read.
func (e Env) gateLog(name string) string {
	if name == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(e.Log.Dir, name))
	if err != nil {
		return ""
	}
	return string(raw)
}
