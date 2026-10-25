package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/doctor"
)

func TestParseDoctorArgs(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		offline, help bool
		wantErr       bool
	}{
		{nil, false, false, false},
		{[]string{"--offline"}, true, false, false},
		{[]string{"--help"}, false, true, false},
		{[]string{"-h"}, false, true, false},
		{[]string{"--json"}, false, false, true},
		{[]string{"extra"}, false, false, true},
	} {
		offline, help, err := parseDoctorArgs(tc.args)
		if offline != tc.offline || help != tc.help || (err != nil) != tc.wantErr {
			t.Errorf("%v: offline=%v help=%v err=%v", tc.args, offline, help, err)
		}
	}
}

// A partial install is exactly what doctor is for, so it names every stage
// executable it cannot find instead of letting the first dispatch discover one.
func TestInstallCheckNamesEveryMissingExecutable(t *testing.T) {
	dir := t.TempDir()
	for _, c := range commands {
		if c.File != "" && c.Name != "watch" && c.Name != "review" {
			writeExec(t, filepath.Join(dir, c.File))
		}
	}
	r := checkInstall([]string{dir}, isExecutable)
	if r.Status != doctor.Fail {
		t.Fatalf("install: %s, want fail", r.Status)
	}
	for _, missing := range []string{"watch", "review"} {
		if !strings.Contains(r.Detail, missing) {
			t.Errorf("detail %q does not name %s", r.Detail, missing)
		}
	}
	if !strings.Contains(r.Fix, installCommand) {
		t.Errorf("fix %q is not the reinstall command", r.Fix)
	}

	writeExec(t, filepath.Join(dir, "watch"))
	writeExec(t, filepath.Join(dir, "review"))
	if r := checkInstall([]string{dir}, isExecutable); r.Status != doctor.OK {
		t.Errorf("complete install: %s %q, want ok", r.Status, r.Detail)
	}
}

func TestDoctorHelpAndBadFlagsExitWithoutChecking(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runDoctor([]string{"--help"}, nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "--offline") {
		t.Errorf("--help: exit %d, stdout %q", code, out.String())
	}
	out.Reset()
	if code := runDoctor([]string{"--bogus"}, nil, &out, &errOut); code != 2 {
		t.Errorf("--bogus: exit %d, want 2 (usage error)", code)
	}
}
