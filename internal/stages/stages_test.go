package stages

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// BEH-571: fixSessionError maps a finished auto-fix session's outcome to the
// error its ci.Driver.Fix callback returns. A spending-cap abort is its own
// retry-after-reset class (ci.ErrSpendingCapActive), distinct from a generic
// non-zero exit, and takes precedence over the exit code (a cap abort also exits
// non-zero) so a cap-active window isn't mislabelled as a fix-attempt failure.
func TestFixSessionErrorSpendingCapTakesPrecedence(t *testing.T) {
	// Cap aborts also carry a non-zero exit; the cap class must win.
	err := fixSessionError(session.Outcome{ExitCode: 1, SpendingCapAbort: true}, 1)
	if !errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("err = %v, want ci.ErrSpendingCapActive", err)
	}
}

// newProvisionLog opens a real runlog over a temp dir, so the narration the
// provisioning path emits has somewhere to go.
func newProvisionLog(t *testing.T, identifier string) *runlog.Logger {
	t.Helper()
	log, err := runlog.New(t.TempDir(), identifier)
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return log
}

// BEH-636/BEH-796: provisionWorktree creates the worktree host-side only when it is
// absent, but it runs post_create EVERY time — including on a worktree that already
// exists. Skipping post_create on the existing path (the original BEH-636 shape) hands
// the session a tree whose deps are absent: the successful-handoff strip (BEH-412)
// removes the Consumer's build artifacts, and an OOM-killed install (BEH-523) never
// wrote them. Worse, such a worktree still carries any readiness sentinel a prior run
// left, which asserts readiness the tree no longer has (BEH-549 defines it as "the run
// finished", not "the install succeeded"), so the session only discovers the gap on its
// first failed test. Re-running post_create is safe because a Consumer's toolchain hook
// is required to be idempotent.
func TestProvisionWorktreeReprovisionsExistingWorktree(t *testing.T) {
	h := hostio.NewFake()
	h.Exists = true
	cfg := config.Config{Project: config.Project{BranchPrefix: "feat", PostCreate: "cd web && pnpm install"}}

	if err := provisionWorktree(h, cfg, "beh-796-x", newProvisionLog(t, "BEH-796")); err != nil {
		t.Fatalf("provisionWorktree on an existing worktree: %v", err)
	}
	if len(h.Shells) != 1 || h.Shells[0].Label != postCreateStep {
		t.Errorf("post_create must re-run on an existing worktree — a resumed tree can be stripped of its deps (BEH-796); shells = %+v", h.Shells)
	}
	for _, c := range h.Calls {
		if strings.HasPrefix(c, "create-worktree") {
			t.Error("an existing worktree must not be re-created")
		}
	}
}

// The fresh-ticket path is unchanged by BEH-796: an absent worktree is created
// host-side and then provisioned, in that order.
func TestProvisionWorktreeCreatesThenProvisionsFreshWorktree(t *testing.T) {
	h := hostio.NewFake()
	h.Exists = false
	cfg := config.Config{Project: config.Project{BranchPrefix: "feat", PostCreate: "cd web && pnpm install"}}

	if err := provisionWorktree(h, cfg, "beh-796-fresh", newProvisionLog(t, "BEH-796")); err != nil {
		t.Fatalf("provisionWorktree on a fresh worktree: %v", err)
	}
	want := []string{"create-worktree beh-796-fresh", "shell " + postCreateStep}
	if !reflect.DeepEqual(h.Calls, want) {
		t.Errorf("calls = %v, want the worktree created host-side and then provisioned, in that order (%v)", h.Calls, want)
	}
}

// A failed creation is fatal and must short-circuit: there is no tree to provision,
// so running post_create against a path that does not exist would only bury the real
// error under a container failure. This is the edge the BEH-796 restructure could
// regress — post_create is no longer nested under the creation branch.
func TestProvisionWorktreeSkipsPostCreateWhenCreationFails(t *testing.T) {
	h := hostio.NewFake()
	h.Exists = false
	h.CreateErr = errors.New("git worktree add: boom")
	cfg := config.Config{Project: config.Project{BranchPrefix: "feat", PostCreate: "cd web && pnpm install"}}

	err := provisionWorktree(h, cfg, "beh-796-broken", newProvisionLog(t, "BEH-796"))
	if err == nil {
		t.Fatal("a failed worktree creation must be fatal")
	}
	if !strings.Contains(err.Error(), "git worktree add: boom") {
		t.Errorf("err = %v, want it to wrap the underlying creation failure", err)
	}
	if len(h.Shells) != 0 {
		t.Errorf("post_create must not run when there is no worktree to provision, got %+v", h.Shells)
	}
}

// An empty post_create is a supported Consumer config ("no setup step"), and the
// BEH-796 restructure lifted that guard out from under the creation branch — so pin
// it: an unconfigured hook must still be skipped on both provisioning paths.
func TestProvisionWorktreeSkipsUnconfiguredPostCreate(t *testing.T) {
	h := hostio.NewFake()
	h.Exists = false
	cfg := config.Config{Project: config.Project{BranchPrefix: "feat"}}

	if err := provisionWorktree(h, cfg, "beh-796-nohook", newProvisionLog(t, "BEH-796")); err != nil {
		t.Fatalf("provisionWorktree with no post_create: %v", err)
	}
	if len(h.Calls) != 1 || h.Calls[0] != "create-worktree beh-796-nohook" {
		t.Errorf("an absent worktree must still be created when there is no post_create hook; calls = %v", h.Calls)
	}
	if len(h.Shells) != 0 {
		t.Errorf("an empty post_create must not be run, got %+v", h.Shells)
	}
}

func TestFixSessionErrorNonZeroExitIsGenericFailure(t *testing.T) {
	err := fixSessionError(session.Outcome{ExitCode: 2}, 3)
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if errors.Is(err, ci.ErrSpendingCapActive) {
		t.Fatalf("a plain non-zero exit must not be the spending-cap class: %v", err)
	}
	if !strings.Contains(err.Error(), "exited 2") || !strings.Contains(err.Error(), "session 3") {
		t.Fatalf("err %q should name the attempt and exit code", err)
	}
}

func TestFixSessionErrorCleanSessionReturnsNil(t *testing.T) {
	if err := fixSessionError(session.Outcome{ExitCode: 0}, 1); err != nil {
		t.Fatalf("a clean session must return nil, got %v", err)
	}
}

// BEH-634: runGates iterates the config-declared named gate list host-side,
// running each gate in order and stopping at the first non-green one. When every
// gate passes it runs them all and reports no failing gate; when one fails it
// reports that gate's name and does not run the gates after it (the push gate
// only needs the first red to withhold the push, and skipping the rest saves a
// container launch). The failing gate's name is what flows into the log + CI-fix
// diagnosis, matching how internal/ci names a failing check.
func TestRunGatesAllGreenRunsEveryGateInOrder(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	var ran []string
	res := runGates(gates, func(g config.Gate) session.Outcome {
		ran = append(ran, g.Name)
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "" {
		t.Errorf("all-green gates must report no failing gate, got %q", res.FailedGate)
	}
	if res.Outcome.ExitCode != 0 {
		t.Errorf("all-green outcome must be exit 0, got %d", res.Outcome.ExitCode)
	}
	if !reflect.DeepEqual(ran, []string{"check", "typecheck"}) {
		t.Errorf("gates must run in config order, ran %v", ran)
	}
}

func TestRunGatesStopsAtFirstFailureAndReportsItsName(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	var ran []string
	res := runGates(gates, func(g config.Gate) session.Outcome {
		ran = append(ran, g.Name)
		if g.Name == "check" {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "check" {
		t.Errorf("the first red gate must be reported by name, got %q", res.FailedGate)
	}
	if res.Outcome.ExitCode != 2 {
		t.Errorf("the failing gate's outcome (exit 2) must be returned, got %d", res.Outcome.ExitCode)
	}
	if !reflect.DeepEqual(ran, []string{"check"}) {
		t.Errorf("gates after the first failure must not run, ran %v", ran)
	}
}

func TestRunGatesReportsLaterFailingGateByName(t *testing.T) {
	gates := []config.Gate{
		{Name: "check", Command: "pnpm run check"},
		{Name: "typecheck", Command: "pnpm run typecheck"},
	}
	res := runGates(gates, func(g config.Gate) session.Outcome {
		if g.Name == "typecheck" {
			return session.Outcome{ExitCode: 1}
		}
		return session.Outcome{ExitCode: 0}
	})
	if res.FailedGate != "typecheck" {
		t.Errorf("the failing gate (typecheck) must be reported by name, got %q", res.FailedGate)
	}
}

// BEH-553: hasUpstreamTranscripts is the retrospective's host-side precondition
// that an upstream /tdd or /review session actually ran and left something to
// mine. It counts implementation-*.jsonl and review-*.jsonl transcripts under the
// ticket log dir; the retrospective's OWN transcripts must not count (else a
// re-run of a misscheduled retrospective would self-satisfy the gate).
func TestHasUpstreamTranscripts(t *testing.T) {
	write := func(dir, name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	t.Run("empty dir has none", func(t *testing.T) {
		if hasUpstreamTranscripts(t.TempDir()) {
			t.Error("an empty log dir has no upstream transcripts")
		}
	})

	t.Run("only retrospective transcripts do not count", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "retrospective-20260625-195139.jsonl")
		write(dir, "run.jsonl")
		if hasUpstreamTranscripts(dir) {
			t.Error("the retrospective's own logs must not satisfy its precondition")
		}
	})

	t.Run("an implementation transcript counts", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "implementation-20260625-100000.jsonl")
		if !hasUpstreamTranscripts(dir) {
			t.Error("an implementation transcript is an upstream session")
		}
	})

	t.Run("a review transcript counts", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "review-20260625-110000.jsonl")
		if !hasUpstreamTranscripts(dir) {
			t.Error("a review transcript is an upstream session")
		}
	})

	t.Run("a retry-suffixed implementation transcript counts", func(t *testing.T) {
		dir := t.TempDir()
		write(dir, "implementation-retry2-20260625-120000.jsonl")
		if !hasUpstreamTranscripts(dir) {
			t.Error("a retry/launch-suffixed implementation transcript still counts")
		}
	})

	t.Run("a missing dir has none", func(t *testing.T) {
		if hasUpstreamTranscripts(filepath.Join(t.TempDir(), "does-not-exist")) {
			t.Error("a non-existent log dir must read as no transcripts, not panic")
		}
	})
}

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
// fresh attempt — an environmental session crash (the 125/137 launch retries
// exhausted, or an idle-timeout kill mid-session) — from a run that completed and
// produced no diff, or a spending-cap abort (which has its own retry-after-reset
// handling). It keys on the session outcome, NOT WorktreeExists: the harness now
// pre-creates the worktree host-side (BEH-636), so a crashed session still leaves
// WorktreeExists == true and the old worktree-existence proxy is permanently
// false. The pipeline re-attempts only the env-crash case (BEH-543/BEH-707).
func TestRetryableEnvCrash(t *testing.T) {
	// The regression case: a pre-provisioned worktree is present, yet the session
	// crashed environmentally (137 OOM / idle-timeout kill after the launch retries
	// exhausted). Must still be retryable — the worktree existing no longer means
	// the session got anywhere.
	if !retryableEnvCrash(session.Outcome{ExitCode: sandbox.ExitOOMKill}, false) {
		t.Error("an environmental crash (137) is retryable even with a pre-provisioned worktree present")
	}
	// A clean, non-crash no-op session (agent ran, committed nothing, exited 0) is
	// NOT an environmental crash — preserve the pre-BEH-636 behaviour of not
	// releasing/retrying it.
	if retryableEnvCrash(session.Outcome{ExitCode: 0}, false) {
		t.Error("a clean exit-0 no-op session is NOT an environmental crash — must not retry")
	}
	// A watchdog cap-kill exits 137 like an OOM but ran a full session of real
	// work; it has checkpoint-commit handling, so it must not be treated as a bare
	// environmental crash.
	if retryableEnvCrash(session.Outcome{ExitCode: sandbox.ExitOOMKill, CapKilled: true}, false) {
		t.Error("a watchdog cap-kill (137 + CapKilled) ran a full session — not an environmental crash")
	}
	// A spending-cap abort has its own retry-after-reset handling — must not be
	// reported retryable here even when the outcome otherwise looks transient.
	if retryableEnvCrash(session.Outcome{ExitCode: sandbox.ExitOOMKill}, true) {
		t.Error("a spending-cap abort has its own handling — must not be reported retryable here")
	}
	// A deterministic zero-work crash (a prompt-expansion no-op that billed $0 —
	// BEH-691) is NOT an environmental transient: re-launching the identical prompt
	// fails identically, so the pipeline must not spend BEH-543's stage retry on it.
	if retryableEnvCrash(session.Outcome{ExitCode: sandbox.ExitOOMKill, NoRealTurns: true}, false) {
		t.Error("a deterministic no-real-turns crash must not be re-attempted (BEH-691)")
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

// diskFullWarning is the actionable warning both stages log when the findings-dir
// mkdir hits ENOSPC (BEH-540). It must name the stage, carry the underlying error,
// and — crucially — append the SHARED reclaim hint so the Docker-cache reclaims
// (the harness's usual disk hog, BEH-566) stay in sync with the Preflight floor
// error. Asserting against sandbox.DiskReclaimHint (not a copied literal) is what
// guarantees the three sites can never drift.
func TestDiskFullWarning(t *testing.T) {
	msg := diskFullWarning("retrospective", &os.PathError{Op: "mkdir", Path: "/x/findings", Err: syscall.ENOSPC})

	for _, want := range []string{
		"retrospective",           // names the stage
		"disk full",               // the condition
		"no space left on device", // the underlying error, surfaced
		sandbox.DiskReclaimHint,   // the shared remediation, verbatim
		"docker builder prune",    // …which now includes the Docker reclaim
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diskFullWarning should contain %q, got: %q", want, msg)
		}
	}
}

func TestParseArgsValidIdentifierUppercases(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"beh-527"}, false)
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
	got, err := ParseArgs("review", []string{"--verbose", "BEH-1", "--dry-run"}, false)
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
	_, err := ParseArgs("retrospective", []string{"--verbose"}, false)
	if err == nil {
		t.Fatal("ParseArgs accepted argv with no ticket id, want error")
	}
	if want := "retrospective"; !strings.Contains(err.Error(), want) {
		t.Errorf("usage error %q does not name the tool %q", err.Error(), want)
	}
}

func TestParseArgsRejectsNonTicketToken(t *testing.T) {
	if _, err := ParseArgs("implementation", []string{"not-a-ticket"}, false); err == nil {
		t.Error("ParseArgs accepted a non-ticket token, want error")
	}
}

// BEH-565: `pipeline --next` auto-selects a ticket, so it parses with no
// identifier and sets Next. allowNext gates the flag to the pipeline (the three
// standalone tools never select).
func TestParseArgsNextFlagParsesWithoutIdentifier(t *testing.T) {
	got, err := ParseArgs("pipeline", []string{"--next"}, true)
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if !got.Next {
		t.Error("expected --next to set Next=true")
	}
	if got.Identifier != "" {
		t.Errorf("Identifier = %q, want empty (--next supplies no id)", got.Identifier)
	}
}

// --next composes with --dry-run (resolve + preview without claiming).
func TestParseArgsNextWithDryRun(t *testing.T) {
	got, err := ParseArgs("pipeline", []string{"--next", "--dry-run"}, true)
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if !got.Next || !got.DryRun {
		t.Errorf("flags = {Next:%v DryRun:%v}, want both true", got.Next, got.DryRun)
	}
}

// Bare `pipeline` (no id, no --next) keeps the friendly usage error — auto-select
// must be opted into explicitly, never triggered by omission.
func TestParseArgsBarePipelineStillErrors(t *testing.T) {
	if _, err := ParseArgs("pipeline", []string{}, true); err == nil {
		t.Error("bare pipeline with no ticket and no --next must error")
	}
}

// --next together with an explicit ticket id is a conflict: name a ticket OR ask
// for the next one, never both.
func TestParseArgsNextWithExplicitIdentifierIsRejected(t *testing.T) {
	_, err := ParseArgs("pipeline", []string{"--next", "BEH-1"}, true)
	if err == nil {
		t.Fatal("--next + an explicit ticket id must be rejected")
	}
	if !strings.Contains(err.Error(), "BEH-1") {
		t.Errorf("conflict error should name the conflicting id, got %q", err.Error())
	}
}

// --next is pipeline-only: a standalone tool (allowNext=false) must not accept it
// — it falls through to the missing-identifier usage error rather than selecting.
func TestParseArgsNextRejectedWhenNotAllowed(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"--next"}, false)
	if err == nil {
		t.Fatal("--next must be rejected for a tool that does not allow selection")
	}
	if got.Next {
		t.Error("Next must stay false when --next is not allowed")
	}
}

// BEH-528: --force is the human-confirmation escape hatch for the already-merged
// dispatch guard — it lets an operator override a false positive (a key that only
// coincidentally appears in a follow-up commit) without editing the harness. It
// parses alongside the identifier and defaults off so the guard stays armed.
func TestParseArgsForceFlag(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"BEH-528", "--force"}, false)
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
// work: the ProjectPath is a git repo with no feat/ branch and the log dir is empty.
func TestRetrospectiveSkipsWhenNoPipelineInputs(t *testing.T) {
	herd := t.TempDir()
	runGitForTest(t, herd, "init", "-q", "-b", "main")

	logDir := filepath.Join(herd, "agent-harness", "logs", "BEH-318")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("setup log dir: %v", err)
	}
	log := &runlog.Logger{Dir: logDir}

	h := hostio.NewFake()
	h.BranchThere = false // no feat/ branch: the /tdd step produced nothing

	res := Retrospective(h, config.Config{Host: config.Host{ProjectPath: herd}}, log, Args{Identifier: "BEH-318"})

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
	got, err := ParseArgs("implementation", []string{"BEH-528"}, false)
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Force {
		t.Error("Force must default to false")
	}
}

// tddCap grants the larger active-time cap to a multi-file extract-and-rewire
// refactor (BEH-688 Symptom 2) and the ordinary cap to everything else.
func TestTddCap(t *testing.T) {
	cfg := config.Config{
		Host: config.Host{
			TddTimeout:              30 * time.Minute,
			TddLargeRefactorTimeout: 60 * time.Minute,
		},
	}
	large := ticket.Ticket{
		Title:       "Extract shared admin scaffolding",
		Description: "Extract 5 modules, then rewire 4 managers to consume them.",
	}
	if got := tddCap(cfg, large); got != cfg.TddLargeRefactorTimeout {
		t.Errorf("tddCap(large refactor) = %v, want %v", got, cfg.TddLargeRefactorTimeout)
	}
	small := ticket.Ticket{Title: "Add a lint guard", Description: "One rule plus tests."}
	if got := tddCap(cfg, small); got != cfg.TddTimeout {
		t.Errorf("tddCap(ordinary ticket) = %v, want %v", got, cfg.TddTimeout)
	}
}

// `--help` is the first thing an operator who downloaded a release archive types.
// It must reach usage without a credential, a config file or a tracker call:
// exiting 1 on "missing Claude credential" teaches nothing about what the tool
// takes, and a new operator cannot tell a usage mistake from a setup mistake.
func TestParseArgsHelpRequestsUsage(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		if _, err := ParseArgs("pipeline", []string{flag}, true); !errors.Is(err, ErrHelp) {
			t.Errorf("ParseArgs(%q) error = %v, want ErrHelp", flag, err)
		}
	}
}

// Help wins over everything else on the line, so `pipeline BEH-1 --help` explains
// itself rather than starting a run over BEH-1.
func TestParseArgsHelpOutranksAnIdentifier(t *testing.T) {
	if _, err := ParseArgs("pipeline", []string{"BEH-1", "--help"}, true); !errors.Is(err, ErrHelp) {
		t.Errorf("error = %v, want ErrHelp", err)
	}
}

// The usage line names the tool it was asked about, and the pipeline-only --next.
func TestUsageNamesToolAndNext(t *testing.T) {
	u := Usage("pipeline", true)
	if !strings.Contains(u, "pipeline") {
		t.Errorf("usage must name the tool: %q", u)
	}
	if !strings.Contains(u, "--next") {
		t.Errorf("usage must document --next when it is allowed: %q", u)
	}
	if u := Usage("review", false); strings.Contains(u, "--next") {
		t.Errorf("usage must not offer --next to a tool that rejects it: %q", u)
	}
}

// `--version` answers before any credential too, because the first thing an
// operator does when a run misbehaves is check which binary they are on — and a
// Consumer's min_harness_version pin is meaningless if you cannot read it.
func TestParseArgsVersionRequestsVersion(t *testing.T) {
	if _, err := ParseArgs("pipeline", []string{"--version"}, true); !errors.Is(err, ErrVersion) {
		t.Errorf("error = %v, want ErrVersion", err)
	}
}

// Help outranks version, so `--help --version` explains the tool rather than
// printing a number and exiting.
func TestParseArgsHelpOutranksVersion(t *testing.T) {
	if _, err := ParseArgs("pipeline", []string{"--version", "--help"}, true); !errors.Is(err, ErrHelp) {
		t.Errorf("error = %v, want ErrHelp", err)
	}
}
