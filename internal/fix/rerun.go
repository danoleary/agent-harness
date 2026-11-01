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
// connection closing under a test, a process killed for memory, a toolchain or
// package download failing on the network. Matched case-insensitively.
var infraSignatures = []struct{ cause, needle string }{
	{"the test browser's connection closed", "browser connection was closed"},
	{"the test browser's connection closed", "rpc is closed"},
	{"the test browser's connection closed", "page closed unexpectedly"},
	{"the test browser's connection closed", "browser has disconnected"},
	{"the test browser's connection closed", "target page, context or browser has been closed"},
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
	for _, s := range infraSignatures {
		if strings.Contains(text, s.needle) {
			return s.cause
		}
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
