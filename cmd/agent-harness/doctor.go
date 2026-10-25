package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/danoleary/agent-harness/internal/doctor"
)

const doctorUsage = `usage: agent-harness doctor [--offline]

Checks the whole setup in one pass — the host tools, the .env that was read, every
credential (with a live, read-only API call), the project's config and prompt
bodies, the tracker's repo/team/project and labels, and the sandbox image — and
prints a fix under every failing line. Exits 1 if any check fails.

  --offline   skip every network call; the lines that need one say so
`

// runDoctor is `agent-harness doctor`: parse its flags, run every check, print
// the checklist, and return the exit code — 0 only if nothing failed, so it works
// in a script.
func runDoctor(args, dirs []string, stdout, stderr io.Writer) int {
	offline, help, err := parseDoctorArgs(args)
	if help {
		fmt.Fprint(stdout, doctorUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "agent-harness doctor: %v\n\n%s", err, doctorUsage)
		return 2
	}
	results := append([]doctor.Result{checkInstall(dirs, isExecutable)}, doctor.Run(doctor.Live(offline))...)
	doctor.Render(stdout, results)
	if doctor.Failed(results) {
		return 1
	}
	return 0
}

func parseDoctorArgs(args []string) (offline, help bool, err error) {
	for _, a := range args {
		switch a {
		case "--offline":
			offline = true
		case "--verbose", "--dry-run":
			// Every command takes these (see usage); doctor is read-only and
			// already prints everything, so they change nothing.
		case "--help", "-h":
			return false, true, nil
		default:
			return false, false, fmt.Errorf("unknown argument %q", a)
		}
	}
	return offline, false, nil
}

// checkInstall resolves every stage executable the dispatch table names, the way
// a real dispatch would, and names each one it cannot find.
func checkInstall(dirs []string, isExec func(string) bool) doctor.Result {
	var missing []string
	for _, c := range commands {
		if c.File == "" {
			continue
		}
		if _, err := resolve(c, dirs, isExec); err != nil {
			missing = append(missing, c.File)
		}
	}
	if len(missing) > 0 {
		return doctor.Result{
			Name:   "install",
			Status: doctor.Fail,
			Detail: fmt.Sprintf("cannot find %s in:\n%s", strings.Join(missing, ", "), strings.Join(dirs, "\n")),
			Fix:    "reinstall: " + installCommand,
		}
	}
	return doctor.Result{Name: "install", Status: doctor.OK, Detail: "every stage executable resolves"}
}
