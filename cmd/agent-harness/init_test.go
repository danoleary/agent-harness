package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `init` is the one command the dispatcher answers itself: there is no stage
// binary for it, so a broken libexec dir must not stop a new Consumer starting.
func TestInitIsABuiltinNotAStageBinary(t *testing.T) {
	cmd, ok := lookup("init")
	if !ok {
		t.Fatal("init is not in the dispatch table")
	}
	if cmd.Builtin == nil || cmd.File != "" {
		t.Errorf("init = %+v, want a builtin with no executable", cmd)
	}
}

func TestRunInitScaffoldsFromFlags(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code := runInit([]string{"--yes", "--toolchain", "go", "--repo", "acme/widgets"}, dir, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".agent-harness", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `repo = "acme/widgets"`) {
		t.Errorf("config.toml does not carry --repo:\n%s", raw)
	}
}

func TestRunInitHelpIsASuccess(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runInit([]string{"--help"}, t.TempDir(), strings.NewReader(""), &out, &errOut); code != 0 {
		t.Errorf("init --help exit %d, want 0", code)
	}
	for _, want := range []string{"--yes", "--force", "--toolchain", "--repo"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("init --help does not mention %s:\n%s", want, out.String())
		}
	}
}

func TestRunInitRefusalExitsNonZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runInit([]string{"--yes"}, t.TempDir(), strings.NewReader(""), &out, &errOut); code == 0 {
		t.Error("init --yes in an empty dir exit 0, want a refusal")
	}
	if !strings.Contains(errOut.String(), "--toolchain") {
		t.Errorf("refusal does not say what to pass: %s", errOut.String())
	}
}

// The dispatcher's `init` row is the first thing a new Consumer reads, so it must
// advertise every flag init actually defines, not a subset that drifted (#119).
func TestInitRowAdvertisesEveryInitFlag(t *testing.T) {
	cmd, _ := lookup("init")
	var out, errOut bytes.Buffer
	if code := runInit([]string{"--help"}, t.TempDir(), strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("init --help exit %d: %s", code, errOut.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "  -") {
			continue
		}
		name := strings.Fields(strings.TrimPrefix(line, "  -"))[0]
		if len(name) == 1 {
			continue // a shorthand, e.g. -y for --yes
		}
		if !strings.Contains(cmd.Args, "--"+name) {
			t.Errorf("init row %q does not advertise --%s", cmd.Args, name)
		}
	}
}
