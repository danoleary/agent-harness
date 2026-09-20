// Command agent-harness is the one name the harness puts on an operator's PATH.
// It dispatches a subcommand to the stage binary that implements it —
// `agent-harness pipeline BEH-362` execs the `pipeline` binary with the rest of
// the line.
//
// It exists because the six tools are named for their ROLE (loop, watch, review,
// pipeline, implementation, retrospective), and those names are far too generic
// to live in a shared bin dir: `watch` shadows procps' watch(1), and the other
// five are words any project might already have on its PATH. Installing them
// directly is a change to the operator's machine they did not ask for. So the
// install puts the stage binaries in a PRIVATE libexec dir and this dispatcher on
// PATH alone — one name added, nothing shadowed.
//
// Dispatch is syscall.Exec, not a child process: the stage binary REPLACES this
// one, so the terminal, the signal handling and the exit code are exactly what
// they were before the dispatcher existed. That matters — `loop` reads a first
// Ctrl-C as "stop after this ticket" and a second as a hard abort, and `watch`
// only renders its dashboard on a real TTY. A forwarding parent would have to
// reimplement both, and would get them subtly wrong.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/danoleary/agent-harness/internal/version"
)

// command is one dispatchable subcommand: the word an operator types, the
// executable that implements it, and how it reads in `--help`.
type command struct {
	// Name is the subcommand as typed.
	Name string
	// File is the executable's filename, looked up across searchDirs. It differs
	// from Name only for `start`, which is the shipped loop-start.sh launcher
	// rather than a Go binary.
	File string
	// Args is the argument grammar shown in the help column ("" for none).
	Args string
	// Summary is the one-line description in the help listing.
	Summary string
}

// commands is the dispatch table, in the order --help lists them: the two
// everyday entrypoints first, then the single-ticket pipeline, then the three
// individual stages it chains.
var commands = []command{
	{Name: "start", File: "loop-start.sh", Summary: "start the loop as a detached daemon (logs to loop.log)"},
	{Name: "loop", File: "loop", Summary: "work the whole ready queue in the foreground until stopped"},
	{Name: "watch", File: "watch", Args: "[LOGFILE]", Summary: "live, read-only view of the running loop"},
	{Name: "pipeline", File: "pipeline", Args: "<TICKET|--next>", Summary: "one ticket, start to finish, then exit"},
	{Name: "implementation", File: "implementation", Args: "<TICKET>", Summary: "stage 1 alone: /tdd into a worktree + commit"},
	{Name: "review", File: "review", Args: "<TICKET>", Summary: "stage 2 alone: cold review, host gates, push + PR"},
	{Name: "retrospective", File: "retrospective", Args: "<TICKET>", Summary: "stage 3 alone: file findings, tear the worktree down"},
}

// startCommand is the subcommand that runs the shell launcher rather than a Go
// binary, and so is the one that needs the daemon binary's path handed to it.
const startCommand = "start"

// loopCommand is the daemon binary `start` launches.
const loopCommand = "loop"

func main() {
	argv := os.Args[1:]

	// Help is a successful outcome answered before any credential is read — the
	// same contract the stage binaries hold (internal/stages.ErrHelp). A bare
	// `agent-harness` is the first thing a new operator types, so it gets the
	// listing on stdout too rather than a usage error.
	if len(argv) == 0 || argv[0] == "--help" || argv[0] == "-h" || argv[0] == "help" {
		fmt.Print(usage())
		return
	}
	if argv[0] == "--version" || argv[0] == "version" {
		fmt.Println(version.Version)
		return
	}

	cmd, ok := lookup(argv[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "agent-harness: unknown command %q\n\n%s", argv[0], usage())
		os.Exit(2)
	}

	dirs, err := searchDirs(selfDir, os.Getenv("HARNESS_LIBEXEC"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-harness: %v\n", err)
		os.Exit(1)
	}
	path, err := resolve(cmd, dirs, isExecutable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-harness: %v\n", err)
		os.Exit(1)
	}

	env := os.Environ()
	if cmd.Name == startCommand {
		env = withLoopBin(env, dirs, isExecutable)
	}

	// Replace this process. Exec only returns on failure.
	if err := syscall.Exec(path, append([]string{path}, argv[1:]...), env); err != nil {
		fmt.Fprintf(os.Stderr, "agent-harness: cannot run %s: %v\n", path, err)
		os.Exit(1)
	}
}

// lookup finds the command for a typed subcommand name.
func lookup(name string) (command, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return command{}, false
}

// searchDirs returns, in order, the directories a subcommand executable is looked
// for in. The order covers every shape the harness is legitimately laid out in,
// most specific first:
//
//	$HARNESS_LIBEXEC                 an explicit override (and the test seam)
//	<prefix>/libexec/agent-harness   the installed layout: this binary is <prefix>/bin/agent-harness
//	<here>/libexec/agent-harness     a release archive run where it was unpacked
//	<here>                           `make build` (bin/) and `go install` (GOBIN), which are flat
//	<here>/../scripts                a source checkout, for the shell launcher: bin/../scripts
//	<here>/scripts                   a release archive's scripts/
//
// PATH is deliberately NOT searched. A dispatcher that fell back to PATH for a
// subcommand named `watch` or `review` would happily exec whatever unrelated
// program answers to that name — the exact collision this design exists to avoid.
func searchDirs(self func() (string, error), libexecEnv string) ([]string, error) {
	var dirs []string
	if libexecEnv != "" {
		dirs = append(dirs, libexecEnv)
	}
	here, err := self()
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(here)
	return append(dirs,
		filepath.Join(parent, "libexec", "agent-harness"),
		filepath.Join(here, "libexec", "agent-harness"),
		here,
		filepath.Join(parent, "scripts"),
		filepath.Join(here, "scripts"),
	), nil
}

// selfDir is the directory holding this binary, with symlinks resolved — an
// installer or a package manager may well link <prefix>/bin/agent-harness to
// somewhere else, and the stage binaries sit beside the REAL file, not the link.
func selfDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate my own binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe), nil
}

// resolve returns the executable implementing cmd, or an error naming every
// directory that was searched — a missing stage binary means a broken or partial
// install, and "where did you look?" is the only question worth answering.
func resolve(cmd command, dirs []string, isExec func(string) bool) (string, error) {
	for _, dir := range dirs {
		candidate := filepath.Join(dir, cmd.File)
		if isExec(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot find the %q executable (%s). Looked in:\n  %s\n\nReinstall with:\n  %s",
		cmd.Name, cmd.File, strings.Join(dirs, "\n  "), installCommand)
}

// withLoopBin hands the `start` launcher the daemon binary this dispatcher
// resolved, so the shell script does not have to repeat the search — and, more to
// the point, cannot resolve a DIFFERENT `loop` than the one belonging to this
// install. An operator-set LOOP_BIN still wins; the script's own fallbacks cover
// the case where the daemon binary is genuinely missing.
func withLoopBin(env, dirs []string, isExec func(string) bool) []string {
	for _, kv := range env {
		if strings.HasPrefix(kv, "LOOP_BIN=") && kv != "LOOP_BIN=" {
			return env
		}
	}
	loop, ok := lookup(loopCommand)
	if !ok {
		return env
	}
	path, err := resolve(loop, dirs, isExec)
	if err != nil {
		return env
	}
	return append(env, "LOOP_BIN="+path)
}

// isExecutable reports whether path is a file this process can exec. A directory
// named like a command, or a non-executable leftover, reads as absent so the
// search moves on rather than exec'ing something that cannot run.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// installCommand is the one-line install, quoted in the not-found error so an
// operator with a broken install can repair it without going to find the README.
const installCommand = "curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh"

// usage is the whole help surface: what to type, what it does, and where the
// configuration an operator still owes the harness is documented.
func usage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "agent-harness %s — works a backlog autonomously, one ticket at a time.\n\n", version.Version)
	b.WriteString("usage: agent-harness <command> [args…]\n\n")

	// Width the command+args column to the longest entry so the summaries line up
	// however the table is later edited.
	width := 0
	for _, c := range commands {
		if n := len(c.Name + " " + c.Args); n > width {
			width = n
		}
	}
	for _, c := range commands {
		left := c.Name
		if c.Args != "" {
			left += " " + c.Args
		}
		fmt.Fprintf(&b, "  %-*s  %s\n", width, left, c.Summary)
	}
	fmt.Fprintf(&b, "  %-*s  %s\n", width, "--version", "print the harness version")
	fmt.Fprintf(&b, "  %-*s  %s\n", width, "--help", "this message")

	b.WriteString("\nEvery command takes --verbose and --dry-run, and answers --help without a credential.\n")
	b.WriteString("Stop a running loop with `touch $PROJECT_PATH/.agent-harness/STOP`.\n\n")
	b.WriteString("Before the first run you need credentials in a .env file, and an .agent-harness/\n")
	b.WriteString("directory in the project you point it at:\n")
	b.WriteString("  https://github.com/danoleary/agent-harness/blob/main/docs/CONSUMER.md\n")
	return b.String()
}
