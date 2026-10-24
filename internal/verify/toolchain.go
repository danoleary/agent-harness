package verify

import (
	"regexp"
	"strings"

	"github.com/danoleary/agent-harness/internal/session"
)

// The two statuses a POSIX shell returns when it never ran the command at all:
// 127 when the command is not found, 126 when it was found but cannot be executed.
// Neither is something the command itself said about the worktree (#46).
const (
	exitCannotExecute   = 126
	exitCommandNotFound = 127
)

// shellReportRE matches the line a shell writes when it cannot find or execute the
// command it was handed — bash (`bash: line 1: go: command not found`), dash
// (`/bin/sh: 1: go: not found`) — and make's own report for a recipe whose tool is
// missing (`make: golangci-lint: No such file or directory`). It is anchored on the
// reporter, so a test that fails with `open x: no such file or directory` is the
// code under test talking, not the shell, and never matches.
var shellReportRE = regexp.MustCompile(
	`^(?:(?:/usr)?/bin/)?(?:ba|da|z)?sh(?:: line \d+)?(?:: \d+)?: (.+?): (?:command not found|not found|No such file or directory|Permission denied)$` +
		`|^make(?:\[\d+\])?: (.+?): (?:Command not found|No such file or directory|Permission denied)$`,
)

// makeUnrunnableRE matches make's summary when a recipe's shell returned 126/127.
// make itself then exits 2, so the exit code alone reads as an ordinary red gate.
var makeUnrunnableRE = regexp.MustCompile(`^make(?:\[\d+\])?: \*\*\* .*Error 12[67]$`)

// CommandUnrunnable reports whether a worktree command's shell could not find or
// execute the command it was asked to run — exit 127 or 126 — and, when its stderr
// names it, which command that was. This is an environment failure: the command is
// not on PATH (or not executable) in the worktree-command container, whose
// environment differs from the session container's, so nothing an in-sandbox
// session installs can reach it (#46).
//
// It reads the exit code only. A make-wrapped miss exits 2, which for post_create
// stays the ordinary warn-only class (BEH-490); only the gate, whose verdict
// decides a push, also reads the stderr signature (see [gateUnrunnable]).
func CommandUnrunnable(out session.Outcome) (missing string, ok bool) {
	if out.ExitCode != exitCommandNotFound && out.ExitCode != exitCannotExecute {
		return "", false
	}
	missing, _ = missingCommand(out.StderrTail)
	return missing, true
}

// gateUnrunnable is [CommandUnrunnable] widened for the host gate re-run: a gate is
// commonly a make target, and make reports its recipe's 127 as its own exit 2, so
// the shell's or make's report in the stderr tail is enough on its own.
func gateUnrunnable(out session.Outcome) (missing string, ok bool) {
	if out.ExitCode == 0 {
		return "", false
	}
	if missing, ok := CommandUnrunnable(out); ok {
		return missing, true
	}
	return missingCommand(out.StderrTail)
}

// missingCommand scans a stderr tail for the shell's or make's report that it could
// not run a command, returning the command it named. A bare make `Error 127`
// summary with no naming line still counts, with the name unknown.
func missingCommand(stderr string) (missing string, found bool) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if m := shellReportRE.FindStringSubmatch(line); m != nil {
			return m[1] + m[2], true
		}
		if makeUnrunnableRE.MatchString(line) {
			found = true
		}
	}
	return "", found
}
