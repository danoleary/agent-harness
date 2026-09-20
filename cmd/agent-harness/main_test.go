package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The dispatcher's whole job is finding the right executable. Every layout below
// is one the harness actually ships in, and picking the wrong directory means
// either "command not found" on a good install or — far worse — exec'ing an
// unrelated program that happens to answer to `watch` or `review`.
func TestSearchDirsCoversEveryLayout(t *testing.T) {
	self := func() (string, error) { return "/opt/hh/bin", nil }
	dirs, err := searchDirs(self, "")
	if err != nil {
		t.Fatalf("searchDirs: %v", err)
	}
	want := []string{
		"/opt/hh/libexec/agent-harness", // installed
		"/opt/hh/bin/libexec/agent-harness",
		"/opt/hh/bin",         // make build / go install, both flat
		"/opt/hh/scripts",     // a checkout's scripts/, for loop-start.sh
		"/opt/hh/bin/scripts", // a release archive's scripts/
	}
	if len(dirs) != len(want) {
		t.Fatalf("got %d dirs %v, want %d", len(dirs), dirs, len(want))
	}
	for i := range want {
		if dirs[i] != want[i] {
			t.Errorf("dir %d = %q, want %q", i, dirs[i], want[i])
		}
	}
}

// HARNESS_LIBEXEC is the operator's override and must outrank the layout guesses,
// not merely join them.
func TestSearchDirsPutsOverrideFirst(t *testing.T) {
	dirs, err := searchDirs(func() (string, error) { return "/opt/hh/bin", nil }, "/custom/libexec")
	if err != nil {
		t.Fatalf("searchDirs: %v", err)
	}
	if dirs[0] != "/custom/libexec" {
		t.Errorf("dirs[0] = %q, want the HARNESS_LIBEXEC override first", dirs[0])
	}
}

func TestSearchDirsPropagatesSelfError(t *testing.T) {
	_, err := searchDirs(func() (string, error) { return "", errors.New("no /proc/self/exe") }, "")
	if err == nil {
		t.Fatal("want an error when the binary cannot locate itself")
	}
}

// PATH must never be consulted: the subcommand names are generic words, and a
// PATH fallback is exactly how `agent-harness watch` would end up running
// procps' watch(1).
func TestSearchDirsNeverIncludesPath(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	dirs, err := searchDirs(func() (string, error) { return "/opt/hh/bin", nil }, "")
	if err != nil {
		t.Fatalf("searchDirs: %v", err)
	}
	for _, d := range dirs {
		if d == "/usr/bin" || d == "/bin" {
			t.Fatalf("searchDirs returned a PATH entry (%q)", d)
		}
	}
}

func TestResolveTakesTheFirstDirectoryThatHasIt(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "libexec")
	second := filepath.Join(root, "bin")
	writeExec(t, filepath.Join(first, "pipeline"))
	writeExec(t, filepath.Join(second, "pipeline"))

	cmd, _ := lookup("pipeline")
	got, err := resolve(cmd, []string{first, second}, isExecutable)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(first, "pipeline"); got != want {
		t.Errorf("resolve = %q, want the earlier directory %q", got, want)
	}
}

// A non-executable file, or a directory with the right name, is not a command —
// reading either as a hit turns a repairable "not found" into an exec failure.
func TestResolveSkipsNonExecutables(t *testing.T) {
	root := t.TempDir()
	notExec := filepath.Join(root, "first")
	real := filepath.Join(root, "second")
	if err := os.MkdirAll(filepath.Join(notExec, "loop"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(real, "loop"))

	cmd, _ := lookup("loop")
	got, err := resolve(cmd, []string{notExec, real}, isExecutable)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if want := filepath.Join(real, "loop"); got != want {
		t.Errorf("resolve = %q, want %q", got, want)
	}
}

// The not-found error is read by someone whose install is broken, so it has to
// name the places that were searched and how to repair it.
func TestResolveErrorNamesEveryDirectoryAndTheFix(t *testing.T) {
	cmd, _ := lookup("review")
	_, err := resolve(cmd, []string{"/nowhere/a", "/nowhere/b"}, func(string) bool { return false })
	if err == nil {
		t.Fatal("want an error when nothing resolves")
	}
	for _, want := range []string{"/nowhere/a", "/nowhere/b", "install.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// `start` runs the shell launcher, which needs to be told which daemon binary
// belongs to this install rather than re-deriving it from a different layout.
func TestWithLoopBinPointsTheLauncherAtThisInstallsDaemon(t *testing.T) {
	root := t.TempDir()
	writeExec(t, filepath.Join(root, "loop"))

	env := withLoopBin([]string{"HOME=/home/x"}, []string{root}, isExecutable)
	want := "LOOP_BIN=" + filepath.Join(root, "loop")
	if !contains(env, want) {
		t.Errorf("env %v does not carry %q", env, want)
	}
}

func TestWithLoopBinKeepsAnOperatorsOverride(t *testing.T) {
	root := t.TempDir()
	writeExec(t, filepath.Join(root, "loop"))

	env := withLoopBin([]string{"LOOP_BIN=/my/own/loop"}, []string{root}, isExecutable)
	for _, kv := range env {
		if strings.HasPrefix(kv, "LOOP_BIN=") && kv != "LOOP_BIN=/my/own/loop" {
			t.Errorf("overrode the operator's LOOP_BIN with %q", kv)
		}
	}
}

// A missing daemon binary must leave the env alone rather than exporting an empty
// LOOP_BIN, which the launcher would take as an explicit (and unusable) choice
// and report as "not executable at ”".
func TestWithLoopBinStaysSilentWhenTheDaemonIsMissing(t *testing.T) {
	env := withLoopBin([]string{"HOME=/home/x"}, []string{t.TempDir()}, isExecutable)
	for _, kv := range env {
		if strings.HasPrefix(kv, "LOOP_BIN=") {
			t.Errorf("exported %q for a daemon that does not exist", kv)
		}
	}
}

func TestLookupRejectsUnknownCommands(t *testing.T) {
	if _, ok := lookup("deploy"); ok {
		t.Error("lookup accepted a command that is not in the table")
	}
}

// Every command in the table has to appear in --help: an undocumented subcommand
// is one nobody can find, and this is the whole discovery surface for an operator
// who just ran the install one-liner.
func TestUsageListsEveryCommand(t *testing.T) {
	got := usage()
	for _, c := range commands {
		if !strings.Contains(got, c.Name) {
			t.Errorf("usage does not mention %q", c.Name)
		}
		if !strings.Contains(got, c.Summary) {
			t.Errorf("usage does not summarise %q", c.Name)
		}
	}
	for _, want := range []string{"--version", "--help", "CONSUMER.md", "STOP"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
}

// The `start` entry is the one command implemented by a shell script rather than
// a Go binary, and the mismatch is deliberate — pin it so a rename of the shipped
// launcher cannot silently break dispatch.
func TestStartDispatchesToTheShippedLauncher(t *testing.T) {
	cmd, ok := lookup(startCommand)
	if !ok {
		t.Fatalf("%q is not in the dispatch table", startCommand)
	}
	if cmd.File != "loop-start.sh" {
		t.Errorf("start dispatches to %q, want loop-start.sh", cmd.File)
	}
}

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
