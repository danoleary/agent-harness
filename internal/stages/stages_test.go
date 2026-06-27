package stages

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/verify"
)

// runGitForTest runs a git command in dir, failing the test on error. Used to set
// up the host checkout shape (a repo with no feature branch) the skip path reads.
func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// retryableEnvCrash distinguishes the one failed-implementation outcome worth a
// fresh attempt — an environmental crash that left no worktree and no commit —
// from a run that completed and produced no diff, or a spending-cap abort (which
// has its own retry-after-reset handling). The pipeline re-attempts only the
// former (BEH-543).
func TestRetryableEnvCrash(t *testing.T) {
	if !retryableEnvCrash(verify.GroundTruth{WorktreeExists: false}, false) {
		t.Error("no worktree + not cap-aborted is an environmental crash — should be retryable")
	}
	if retryableEnvCrash(verify.GroundTruth{WorktreeExists: true, CommitsAhead: 0}, false) {
		t.Error("a worktree that exists (ran to completion, empty diff) is NOT an environmental crash")
	}
	if retryableEnvCrash(verify.GroundTruth{WorktreeExists: false}, true) {
		t.Error("a spending-cap abort has its own handling — must not be reported retryable here")
	}
}

// isDiskFull recognises the ENOSPC a findings-dir mkdir returns when the host disk
// is full (BEH-540), so the stage can degrade to a clear warning instead of an
// opaque hard error. It must see through os.PathError's wrapping and ignore other
// errno values.
func TestIsDiskFull(t *testing.T) {
	if !isDiskFull(&os.PathError{Op: "mkdir", Path: "/x/findings", Err: syscall.ENOSPC}) {
		t.Error("ENOSPC PathError should be disk-full")
	}
	if !isDiskFull(syscall.ENOSPC) {
		t.Error("bare ENOSPC should be disk-full")
	}
	if isDiskFull(&os.PathError{Op: "mkdir", Path: "/x", Err: syscall.EACCES}) {
		t.Error("EACCES (permission) is not disk-full")
	}
	if isDiskFull(errors.New("boom")) {
		t.Error("a generic error is not disk-full")
	}
	if isDiskFull(nil) {
		t.Error("nil is not disk-full")
	}
}

func TestParseArgsValidIdentifierUppercases(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"beh-527"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Identifier != "BEH-527" {
		t.Errorf("Identifier = %q, want BEH-527 (lowercase input should uppercase)", got.Identifier)
	}
	if got.DryRun || got.Verbose {
		t.Errorf("flags = {DryRun:%v Verbose:%v}, want both false", got.DryRun, got.Verbose)
	}
}

func TestParseArgsFlags(t *testing.T) {
	got, err := ParseArgs("review", []string{"--verbose", "BEH-1", "--dry-run"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Identifier != "BEH-1" {
		t.Errorf("Identifier = %q, want BEH-1", got.Identifier)
	}
	if !got.DryRun || !got.Verbose {
		t.Errorf("flags = {DryRun:%v Verbose:%v}, want both true", got.DryRun, got.Verbose)
	}
}

func TestParseArgsRejectsMissingIdentifierWithToolNameInUsage(t *testing.T) {
	_, err := ParseArgs("retrospective", []string{"--verbose"})
	if err == nil {
		t.Fatal("ParseArgs accepted argv with no ticket id, want error")
	}
	if want := "retrospective"; !strings.Contains(err.Error(), want) {
		t.Errorf("usage error %q does not name the tool %q", err.Error(), want)
	}
}

func TestParseArgsRejectsNonTicketToken(t *testing.T) {
	if _, err := ParseArgs("implementation", []string{"not-a-ticket"}); err == nil {
		t.Error("ParseArgs accepted a non-ticket token, want error")
	}
}

// BEH-528: --force is the human-confirmation escape hatch for the already-merged
// dispatch guard — it lets an operator override a false positive (a key that only
// coincidentally appears in a follow-up commit) without editing the harness. It
// parses alongside the identifier and defaults off so the guard stays armed.
func TestParseArgsForceFlag(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"BEH-528", "--force"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if !got.Force {
		t.Error("expected --force to set Force=true")
	}
	if got.Identifier != "BEH-528" {
		t.Errorf("Identifier should still parse alongside --force, got %q", got.Identifier)
	}
}

// BEH-552: a retrospective dispatched against a ticket whose pipeline produced no
// transcripts and no feature branch (the BEH-318 shape) must short-circuit BEFORE
// launching the sandbox — and before even reaching the Linear fetch — rather than
// burn a full sandbox to conclude "nothing to read". It is a clean no-op skip
// (OK, never a pipeline failure), recorded as a diagnostic in run.jsonl. The test
// is hermetic precisely because the precondition returns before any network/Docker
// work: the HerdPath is a git repo with no feat/ branch and the log dir is empty.
func TestRetrospectiveSkipsWhenNoPipelineInputs(t *testing.T) {
	herd := t.TempDir()
	runGitForTest(t, herd, "init", "-q", "-b", "main")

	logDir := filepath.Join(herd, "agent-harness", "logs", "BEH-318")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("setup log dir: %v", err)
	}
	log := &runlog.Logger{Dir: logDir}

	res := Retrospective(
		config.Config{HerdPath: herd},
		log,
		"20260625-195129",
		Args{Identifier: "BEH-318"},
	)

	if res.Err != nil {
		t.Fatalf("a clean skip must not surface a hard error, got %v", res.Err)
	}
	if !res.OK {
		t.Error("a skip with nothing to study is a clean no-op, not a pipeline failure (want OK)")
	}
	runJSON, err := os.ReadFile(filepath.Join(logDir, "run.jsonl"))
	if err != nil {
		t.Fatalf("read run.jsonl: %v", err)
	}
	if !strings.Contains(string(runJSON), "skip") {
		t.Errorf("run.jsonl should record the skip diagnostic, got: %s", runJSON)
	}
}

func TestParseArgsForceDefaultsOff(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"BEH-528"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Force {
		t.Error("Force must default to false")
	}
}
