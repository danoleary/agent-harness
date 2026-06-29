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

	"github.com/beherd/agent-harness/internal/ci"
	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/session"
	"github.com/beherd/agent-harness/internal/verify"
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

// disjointWorkTrapped recognises the BEH-609 recovery case: a failed tdd verdict
// where the session DID produce a worktree and commit real work, but the branch
// roots at a disjoint history, so the gate fails it even though the diff is
// genuine. That work is recoverable by re-grafting onto a fresh base rather than
// discarding the run and re-launching the same doomed pipeline. It must NOT fire on
// the ordinary failure shapes (no worktree, empty diff, healthy-but-failing).
func TestDisjointWorkTrapped(t *testing.T) {
	if !disjointWorkTrapped(verify.GroundTruth{WorktreeExists: true, CommitsAhead: 3, DisjointHistory: true}) {
		t.Error("worktree + committed work + disjoint history is trapped verified work — should be recoverable")
	}
	if disjointWorkTrapped(verify.GroundTruth{WorktreeExists: true, CommitsAhead: 3, DisjointHistory: false}) {
		t.Error("a healthy (non-disjoint) branch is not the trapped-work case")
	}
	if disjointWorkTrapped(verify.GroundTruth{WorktreeExists: true, CommitsAhead: 0, DisjointHistory: true}) {
		t.Error("a disjoint branch with no commit has no verified work to regraft")
	}
	if disjointWorkTrapped(verify.GroundTruth{WorktreeExists: false, DisjointHistory: true}) {
		t.Error("no worktree means nothing was produced to recover")
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

// fakeClaimReleaser records the Linear claim/release mutations the implementation
// stage's claim path makes, so PreClaimed branching is testable without live Linear.
type fakeClaimReleaser struct {
	moved    []string
	released []string
	moveErr  error
	relErr   error
}

func (f *fakeClaimReleaser) MoveToInProgress(id string) error {
	f.moved = append(f.moved, id)
	return f.moveErr
}

func (f *fakeClaimReleaser) ReleaseToTodo(id string) error {
	f.released = append(f.released, id)
	return f.relErr
}

// fakeEventLog captures narration for assertions.
type fakeEventLog struct{ events []string }

func (f *fakeEventLog) Event(m string) { f.events = append(f.events, m) }

func (f *fakeEventLog) saw(sub string) bool {
	for _, e := range f.events {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

// BEH-565 / ADR-0003: on the hand-passed path (PreClaimed=false) the
// implementation stage claims the ticket itself, exactly as before.
func TestClaimForImplementationClaimsWhenNotPreClaimed(t *testing.T) {
	c := &fakeClaimReleaser{}
	log := &fakeEventLog{}
	if err := claimForImplementation(c, "BEH-1", false, log); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"BEH-1"}; !reflect.DeepEqual(c.moved, want) {
		t.Errorf("moved = %v, want %v (hand-passed path claims itself)", c.moved, want)
	}
}

// On the --next path the ticket is already In Progress from selection, so the
// stage must NOT issue a redundant claim.
func TestClaimForImplementationSkipsWhenPreClaimed(t *testing.T) {
	c := &fakeClaimReleaser{}
	log := &fakeEventLog{}
	if err := claimForImplementation(c, "BEH-2", true, log); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.moved) != 0 {
		t.Errorf("moved = %v, want none (a pre-claimed ticket must not be re-claimed)", c.moved)
	}
}

// A real claim failure on the hand-passed path is propagated (the stage surfaces
// it as a hard Result.Err).
func TestClaimForImplementationPropagatesClaimError(t *testing.T) {
	c := &fakeClaimReleaser{moveErr: errors.New("linear down")}
	if err := claimForImplementation(c, "BEH-3", false, &fakeEventLog{}); err == nil {
		t.Error("expected the MoveToInProgress error to propagate")
	}
}

// ADR-0003 release-on-preflight-failure: a pre-claimed ticket whose Docker
// preflight fails is returned to Todo so it isn't stranded In Progress.
func TestReleaseIfPreClaimedReleasesWhenPreClaimed(t *testing.T) {
	c := &fakeClaimReleaser{}
	log := &fakeEventLog{}
	releaseIfPreClaimed(c, "BEH-4", true, log)
	if want := []string{"BEH-4"}; !reflect.DeepEqual(c.released, want) {
		t.Errorf("released = %v, want %v (pre-claimed + preflight fail → release)", c.released, want)
	}
	if !log.saw("Todo") {
		t.Errorf("expected a release narration mentioning Todo; events = %v", log.events)
	}
}

// On the hand-passed path nothing was claimed before preflight, so there is
// nothing to release — the BEH-316 ordering stays untouched.
func TestReleaseIfPreClaimedNoOpWhenNotPreClaimed(t *testing.T) {
	c := &fakeClaimReleaser{}
	releaseIfPreClaimed(c, "BEH-5", false, &fakeEventLog{})
	if len(c.released) != 0 {
		t.Errorf("released = %v, want none (hand-passed path never claimed before preflight)", c.released)
	}
}

// A release failure is best-effort: it warns but never panics or escalates (the
// preflight error stays the stage's verdict).
func TestReleaseIfPreClaimedWarnsOnReleaseError(t *testing.T) {
	c := &fakeClaimReleaser{relErr: errors.New("linear down")}
	log := &fakeEventLog{}
	releaseIfPreClaimed(c, "BEH-6", true, log)
	if !log.saw("manually") {
		t.Errorf("expected a warning to move the ticket out of In Progress manually; events = %v", log.events)
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
