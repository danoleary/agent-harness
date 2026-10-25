package stages

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// These tests reach the *body* of the implementation stage — the composition that
// assembles the tested predicates (retryableEnvCrash, disjointWorkTrapped,
// claimForImplementation) and acts on them. Before the hostio.Host seam the body
// reached straight for gitpkg/sandbox/session/trackers at package level, so not one
// of its branches was reachable without a Docker daemon and a live tracker.
//
// Git is real here (#28): each test's host is bound to a temp repo, and a session
// that "commits" does so with git from inside AgentFn. A state a test needs — a
// lost worktree, a disjoint root, a provisioning failure — is one git holds.

// fresh is a healthy host for key over a repo with no worktree for the ticket yet:
// the state the implementation stage starts from.
func fresh(t *testing.T, key string) (*hostio.Fake, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	return hostio.NewFake(repo.Checkout(), ticket.Slug(key)), repo
}

// handedOff is a healthy host for key whose worktree carries one committed handoff
// ahead of origin/main: the state implementation hands review.
func handedOff(t *testing.T, key string) (*hostio.Fake, *gittest.Repo) {
	t.Helper()
	h, repo := fresh(t, key)
	repo.Handoff(ticket.Slug(key))
	return h, repo
}

// commitsHandoff is a /tdd session that commits its handoff and exits 0.
func commitsHandoff(h *hostio.Fake, repo *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
	return func(int, hostio.AgentRun) session.Outcome {
		repo.Commit(h.Worktree(), "fix.txt", "the fix\n", gittest.HandoffSubject)
		return session.Outcome{}
	}
}

// stageCfg is a minimal but complete Consumer config: one gate, a post_create hook,
// prompt bodies, and caps. Individual tests override what they are about.
func stageCfg() config.Config {
	return config.Config{
		Host: config.Host{
			ProjectPath:          "/fake/checkout",
			TddTimeout:           30 * time.Minute,
			ReviewTimeout:        30 * time.Minute,
			RetrospectiveTimeout: 20 * time.Minute,
			SessionIdleTimeout:   5 * time.Minute,
		},
		Project: config.Project{
			BranchPrefix: "feat",
			Image:        "example-agent-harness:latest",
			PostCreate:   "pnpm install --frozen-lockfile",
			Gates:        []config.Gate{{Name: "check", Command: "pnpm run check"}},
			Prompts: config.PromptBodies{
				Implement: "implement the ticket",
				Review:    "review the worktree",
				Retro:     "retrospect the run",
			},
		},
	}
}

// stageLog opens a real runlog under a temp dir. The stage bodies write findings
// dirs and narration through it, and those are ordinary local-FS effects the seam
// deliberately does not abstract.
func stageLog(t *testing.T, key string) *runlog.Logger {
	t.Helper()
	log, err := runlog.New(t.TempDir(), key)
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return log
}

func narration(t *testing.T, log *runlog.Logger) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(log.Dir, "run.jsonl"))
	if err != nil {
		t.Fatalf("read run.jsonl: %v", err)
	}
	return string(raw)
}

func labels(runs []hostio.AgentRun) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.Label
	}
	return out
}

// The happy path, end to end: claim → provision → session → ground truth → strip
// the handoff artifacts → file findings. Every one of those is a host-side effect
// the stage no longer performs itself.
func TestImplementationHappyPathClaimsProvisionsAndVerifies(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	cfg := stageCfg()
	cfg.HandoffStripPaths = []string{"node_modules"}
	h.AgentFn = func(call int, run hostio.AgentRun) session.Outcome {
		if err := os.MkdirAll(filepath.Join(h.Worktree().Path(), "node_modules", "esbuild"), 0o755); err != nil {
			t.Fatalf("install: %v", err)
		}
		return commitsHandoff(h, repo)(call, run)
	}

	res := Implementation(h, cfg, stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})

	if !res.OK {
		t.Fatalf("a worktree ahead of main is a passing handoff, got %+v", res)
	}
	if got := h.Trk.Claimed; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("claimed = %v, want the hand-passed ticket claimed exactly once", got)
	}
	if got := labels(h.Agents); len(got) != 1 || got[0] != implementationSession {
		t.Errorf("agent runs = %v, want one /tdd session", got)
	}
	if _, err := os.Stat(filepath.Join(h.Worktree().Path(), "node_modules")); !os.IsNotExist(err) {
		t.Errorf("a passing handoff must strip the Consumer's build artifacts before the reviewer sees it (BEH-412); stat = %v", err)
	}
	if got := h.Filed; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("filed = %v, want the dropbox filed to the tracker after every session (ADR-0001)", got)
	}
}

// ADR-0003 / BEH-316: the preflight runs BEFORE the claim on the hand-passed path,
// so a host that cannot launch a sandbox never strands a ticket In Progress. On the
// --next path selection already claimed it, so a preflight failure must release it
// back to Todo rather than leave a dequeued ticket nobody is working.
func TestImplementationPreflightFailureReleasesASelectedTicket(t *testing.T) {
	h, _ := fresh(t, "PROJ-2")
	h.PreflightErr = errors.New("insufficient free disk to launch a sandbox")

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-2"), Args{Identifier: "PROJ-2", Lease: lease.Held(h.Trk, "PROJ-2")})

	if res.Err == nil || res.Disposition != PreflightAborted {
		t.Fatalf("a refused preflight is an environmental abort, got %+v", res)
	}
	if got := h.Trk.Released; len(got) != 1 || got[0] != "PROJ-2" {
		t.Errorf("released = %v, want the selected ticket returned to Todo (ADR-0003)", got)
	}
	if len(h.Agents) != 0 {
		t.Errorf("nothing may launch after a refused preflight, got %v", labels(h.Agents))
	}
}

func TestImplementationHandPassedPreflightFailureReleasesNothing(t *testing.T) {
	h, _ := fresh(t, "PROJ-3")
	h.PreflightErr = errors.New("docker daemon not reachable")

	Implementation(h, stageCfg(), stageLog(t, "PROJ-3"), Args{Identifier: "PROJ-3"})

	if len(h.Trk.Released) != 0 {
		t.Errorf("released = %v, want nothing — the hand-passed path claims AFTER preflight (BEH-316)", h.Trk.Released)
	}
	if len(h.Trk.Claimed) != 0 {
		t.Errorf("claimed = %v, want nothing claimed behind a refused preflight", h.Trk.Claimed)
	}
}

// BEH-528: a ticket whose work already merged on main is dropped before the image
// build, the claim and the launch — and --force overrides it for the rare false
// positive.
func TestImplementationSkipsWorkAlreadyMergedOnMain(t *testing.T) {
	h, repo := fresh(t, "PROJ-4")
	repo.Merge("fix.txt", "the fix\n", "PROJ-4: the fix")

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-4"), Args{Identifier: "PROJ-4"})

	if !res.OK {
		t.Fatalf("a skip is a clean no-op, got %+v", res)
	}
	if len(h.Trk.Claimed) != 0 || len(h.Agents) != 0 {
		t.Errorf("a skipped ticket must cost no claim and no session; claimed=%v agents=%v", h.Trk.Claimed, labels(h.Agents))
	}

	forced, repo := fresh(t, "PROJ-4")
	repo.Merge("other.txt", "a related change\n", "PROJ-4: a related change")
	forced.AgentFn = commitsHandoff(forced, repo)
	if res := Implementation(forced, stageCfg(), stageLog(t, "PROJ-4"), Args{Identifier: "PROJ-4", Force: true}); !res.OK {
		t.Fatalf("--force must dispatch anyway, got %+v", res)
	}
	if len(forced.Agents) != 1 {
		t.Errorf("--force must still launch the session, got %v", labels(forced.Agents))
	}
}

// BEH-389: a terminal usage-policy refusal that left a worktree but no handoff
// commit is retried ONCE on the same ticket, under a distinct label (so the Runner
// mints a fresh container name and transcript) and with the resume prompt — which
// asserts the worktree already exists rather than recreating it.
func TestImplementationRetriesAUsagePolicyRefusalWithTheResumePrompt(t *testing.T) {
	h, _ := fresh(t, "PROJ-5")
	h.AgentFn = func(call int, _ hostio.AgentRun) session.Outcome {
		return session.Outcome{ExitCode: 1, UsagePolicyRefusal: true}
	}

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-5"), Args{Identifier: "PROJ-5"})

	if res.OK {
		t.Fatal("no handoff commit is a failed verdict")
	}
	want := []string{implementationSession, implementationSession + "-retry2"}
	if got := labels(h.Agents); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("agent runs = %v, want the refusal retried once under a distinct label %v (BEH-389)", got, want)
	}
	if h.Agents[0].Prompt == h.Agents[1].Prompt {
		t.Error("the retry must use the resume prompt, not re-run the from-scratch one")
	}
}

// The refusal retry is gated on a surviving worktree: a refusal with no worktree has
// no diff to recover, and the resume prompt — which asserts the worktree already
// exists and forbids recreating it — would burn a whole session on a false premise.
//
// The gate reads the worktree live, so this is the defence-in-depth path: since
// BEH-636 the harness pre-creates the worktree host-side before the session, and a
// creation failure is fatal upstream, so reaching a refusal verdict with no worktree
// means one vanished mid-run.
func TestImplementationDoesNotRetryARefusalThatLeftNoWorktree(t *testing.T) {
	h, _ := fresh(t, "PROJ-6")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		if err := os.RemoveAll(h.Worktree().Path()); err != nil {
			t.Fatalf("lose the worktree: %v", err)
		}
		return session.Outcome{ExitCode: 1, UsagePolicyRefusal: true}
	}

	Implementation(h, stageCfg(), stageLog(t, "PROJ-6"), Args{Identifier: "PROJ-6"})

	if got := labels(h.Agents); len(got) != 1 {
		t.Errorf("agent runs = %v, want exactly one — there is no worktree to resume", got)
	}
}

// BEH-543/BEH-707: an environmental crash that left the pre-provisioned worktree
// clean has nothing to salvage, so the claim goes back to Todo for a later run to
// re-grab — and the stage reports Retryable so the pipeline re-attempts it once.
func TestImplementationReleasesTheClaimAfterAnEnvironmentalCrash(t *testing.T) {
	h, _ := fresh(t, "PROJ-7")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		return session.Outcome{ExitCode: sandbox.ExitOOMKill}
	}

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-7"), Args{Identifier: "PROJ-7"})

	if res.OK || !res.Retryable {
		t.Fatalf("an environmental crash with no commit is a retryable failure, got %+v", res)
	}
	if got := h.Trk.Released; len(got) != 1 {
		t.Errorf("released = %v, want the claim returned to Todo (BEH-543/BEH-707)", got)
	}
}

// BEH-691: a terminal $0 result means the model was never invoked — deterministic,
// so re-launching the identical prompt fails identically. It must NOT consume the
// environmental-crash retry, and the ticket stays claimed for a human.
func TestImplementationDoesNotRetryAZeroWorkCrash(t *testing.T) {
	h, _ := fresh(t, "PROJ-8")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		return session.Outcome{NoRealTurns: true}
	}

	log := stageLog(t, "PROJ-8")
	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-8"})

	if !strings.Contains(narration(t, log), "zero real turns") {
		t.Errorf("a $0 no-work crash must be named as the crash it is (BEH-691); run.jsonl = %s", narration(t, log))
	}
	if res.Retryable {
		t.Errorf("a deterministic zero-work crash is not the environmental retry class (BEH-691), got %+v", res)
	}
	if len(h.Trk.Released) != 0 {
		t.Errorf("released = %v, want the ticket left In Progress for a human (BEH-691)", h.Trk.Released)
	}
}

// BEH-479/BEH-389: a failed verdict over a DIRTY worktree gets a recovery
// checkpoint commit, so the session's uncommitted diff is a `git log` away instead
// of a bare worktree needing manual rescue. It never flips the verdict.
func TestImplementationCheckpointsUncommittedWorkOnAFailedVerdict(t *testing.T) {
	h, repo := fresh(t, "PROJ-9")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		repo.Dirty(h.Worktree())
		return session.Outcome{ExitCode: 1}
	}

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-9"), Args{Identifier: "PROJ-9"})

	if res.OK {
		t.Fatal("a checkpoint must not flip the verdict — the work is unverified")
	}
	if want := strings.SplitN(gitpkg.CheckpointMessage("PROJ-9", "tdd"), "\n", 2)[0]; repo.Subject(h.Worktree()) != want || !h.Worktree().Clean() {
		t.Errorf("uncommitted work must be checkpoint-committed (BEH-479); HEAD is %q, want %q", repo.Subject(h.Worktree()), want)
	}
	if len(h.Trk.Released) != 0 {
		t.Errorf("a salvageable diff keeps the claim; released = %v", h.Trk.Released)
	}
}

// BEH-609: verified work trapped on a disjoint branch is re-grafted onto a fresh
// base off origin/main and re-verified, rather than discarded to a doomed re-run.
// The regraft is git's: the second ground-truth read sees the branch it rewrote.
func TestImplementationRegraftsWorkTrappedOnADisjointBranch(t *testing.T) {
	h, repo := fresh(t, "PROJ-10")
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
		repo.Disjoint(h.Worktree())
		return session.Outcome{}
	}

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-10"), Args{Identifier: "PROJ-10"})

	if !res.OK {
		t.Fatalf("a clean regraft turns trapped work into a healthy handoff (BEH-609), got %+v", res)
	}
	wt := h.Worktree()
	if wt.DisjointHistory() || !wt.IsRebasedOnto(gitpkg.BaseRef) {
		t.Error("the regrafted branch must share origin/main's history")
	}
	if got := repo.Git(wt.Path(), "show", "HEAD:trapped.txt"); got != "verified work" {
		t.Errorf("the trapped work must survive the regraft, got %q", got)
	}
}

// A provisioning failure means there is no worktree to run against: release the
// claim and report the retryable env class (BEH-543).
func TestImplementationReleasesTheClaimWhenProvisioningFails(t *testing.T) {
	h, repo := fresh(t, "PROJ-11")
	repo.BlockWorktrees()

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-11"), Args{Identifier: "PROJ-11"})

	if res.OK || !res.Retryable {
		t.Fatalf("a failed provisioning is a retryable env failure, got %+v", res)
	}
	if got := h.Trk.Released; len(got) != 1 {
		t.Errorf("released = %v, want the claim released when there is no worktree", got)
	}
	if len(h.Agents) != 0 {
		t.Errorf("no session may launch without a worktree, got %v", labels(h.Agents))
	}
}

// --dry-run stays a pure prompt/command inspector: nothing claimed, nothing
// launched, and the preview comes from the same builder the launch would use.
func TestImplementationDryRunClaimsNothingAndLaunchesNothing(t *testing.T) {
	h, _ := fresh(t, "PROJ-12")

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-12"), Args{Identifier: "PROJ-12", DryRun: true})

	if !res.OK {
		t.Fatalf("a dry run is a success, got %+v", res)
	}
	if len(h.Trk.Claimed) != 0 || len(h.Agents) != 0 || len(h.Shells) != 0 {
		t.Errorf("a dry run must mutate nothing; claimed=%v agents=%v shells=%v", h.Trk.Claimed, labels(h.Agents), h.Shells)
	}
	if h.Worktree().Exists() {
		t.Error("a dry run must not create the worktree")
	}
}

// A tracker that cannot be built (or cannot resolve the ticket) is a hard setup
// error the wrapper prints before exiting — never a silent "not OK".
func TestImplementationSurfacesATrackerFailureAsAHardError(t *testing.T) {
	h, _ := fresh(t, "PROJ-13")
	h.TrackerErr = errors.New("LINEAR_API_KEY is not set")

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-13"), Args{Identifier: "PROJ-13"})

	if res.Err == nil {
		t.Fatalf("a tracker failure is a hard error, got %+v", res)
	}
}

// #25: under the daemon the ticket arrives claimed by selection. A provisioning
// failure releases it to Todo and asks the pipeline to retry; the retry must claim
// it again rather than work a ticket another selector can now grab.
func TestImplementationRetryAfterAReleaseReclaimsTheTicket(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	repo.BlockWorktrees()
	h.AgentFn = commitsHandoff(h, repo)
	args := Args{Identifier: "PROJ-1", Lease: lease.Held(h.Trk, "PROJ-1")}

	first := Implementation(h, stageCfg(), stageLog(t, "PROJ-1"), args)
	if !first.Retryable {
		t.Fatalf("a provisioning failure is a retryable environmental crash, got %+v", first)
	}
	repo.Unblock()
	second := Implementation(h, stageCfg(), stageLog(t, "PROJ-1"), args)

	if !second.OK {
		t.Fatalf("the retry should hand off once provisioning recovers, got %+v", second)
	}
	if got := h.Trk.Released; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("released = %v, want the failed attempt to return PROJ-1 to Todo", got)
	}
	if got := h.Trk.Claimed; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("claimed = %v, want the retry to re-claim PROJ-1 before working it", got)
	}
}

// A claim the tracker refuses is a hard error: nothing is provisioned or launched
// for a ticket this run does not hold.
func TestImplementationClaimFailureLaunchesNothing(t *testing.T) {
	h, _ := fresh(t, "PROJ-1")
	h.Trk.ClaimErr = errors.New("tracker unreachable")

	res := Implementation(h, stageCfg(), stageLog(t, "PROJ-1"), Args{Identifier: "PROJ-1"})

	if res.Err == nil {
		t.Fatalf("a refused claim must surface as Result.Err, got %+v", res)
	}
	if len(h.Agents) != 0 {
		t.Errorf("nothing may launch for an unclaimed ticket, got %v", labels(h.Agents))
	}
}

// #46: a post_create the shell could not run (127 not found, 126 not executable)
// is an environment failure the session cannot fix — it runs in a different
// container whose PATH is fine, so nothing it installs reaches the gate container.
// The stage stops before any session launches and says why.
func TestImplementationAbortsWhenPostCreateCannotRun(t *testing.T) {
	for _, code := range []int{127, 126} {
		h, _ := fresh(t, "PROJ-46")
		h.ShellOutcome = session.Outcome{ExitCode: code, StderrTail: "bash: line 1: pnpm: command not found"}
		log := stageLog(t, "PROJ-46")

		res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-46"})

		if res.OK || res.Retryable {
			t.Errorf("exit %d: an unrunnable post_create is a fatal, non-retryable env failure, got %+v", code, res)
		}
		if len(h.Agents) != 0 {
			t.Errorf("exit %d: no session may launch behind an unrunnable post_create, got %v", code, labels(h.Agents))
		}
		if got := h.Trk.Released; len(got) != 1 {
			t.Errorf("exit %d: released = %v, want the claim returned — nothing was worked", code, got)
		}
		said := narration(t, log)
		for _, want := range []string{"`pnpm`", "not on PATH in the worktree-command container", "bash: line 1: pnpm: command not found"} {
			if !strings.Contains(said, want) {
				t.Errorf("exit %d: run.jsonl should carry %q; got %s", code, want, said)
			}
		}
	}
}

// BEH-490 still holds for every other non-zero exit: a failed install degrades to a
// warning and the session provisions in-session. The OOM-kill keeps its retry.
func TestImplementationPostCreateFailureIsStillWarnOnly(t *testing.T) {
	h, repo := fresh(t, "PROJ-47")
	h.ShellOutcome = session.Outcome{ExitCode: 1, StderrTail: "ERR_PNPM_FETCH_404"}
	h.AgentFn = commitsHandoff(h, repo)
	log := stageLog(t, "PROJ-47")

	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-47"})

	if !res.OK || len(h.Agents) != 1 {
		t.Fatalf("an ordinary post_create failure is warn-only, got %+v with agents %v", res, labels(h.Agents))
	}
	if !strings.Contains(narration(t, log), "post_create exited 1") {
		t.Errorf("the warning must still be logged; run.jsonl = %s", narration(t, log))
	}
	if got := h.Shells[0].Retry.MaxAttempts; got != oomMaxAttempts {
		t.Errorf("post_create retry attempts = %d, want the OOM retry (%d) kept", got, oomMaxAttempts)
	}
}

// #90: a ready-labelled ticket whose body still has a section of questions for a
// human is dispatched anyway (a ready label is a human's call), but the run log
// says so, naming the section — the dispatch advisory, like ResolvedAdvisory.
func TestImplementationAdvisesWhenTheTicketStillListsOpenQuestions(t *testing.T) {
	h, repo := fresh(t, "PROJ-90")
	h.AgentFn = commitsHandoff(h, repo)
	h.Trk.Ticket.Description = "Add `init`.\n\n## Open questions (for a human before this is agent-ready)\n\n- Which trackers?"
	log := stageLog(t, "PROJ-90")

	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-90"})

	if !res.OK || len(h.Agents) != 1 {
		t.Fatalf("an open-questions advisory must never skip the dispatch, got %+v with runs %v", res, labels(h.Agents))
	}
	if got := narration(t, log); !strings.Contains(got, "still lists open questions") || !strings.Contains(got, "Open questions (for a human before this is agent-ready)") {
		t.Errorf("run log does not name the open-questions section:\n%s", got)
	}
}

func TestImplementationIsQuietWhenTheTicketHasNoOpenQuestions(t *testing.T) {
	h, _ := fresh(t, "PROJ-91")
	log := stageLog(t, "PROJ-91")

	Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-91"})

	if got := narration(t, log); strings.Contains(got, "open questions") {
		t.Errorf("a ticket with no open-questions section logged an advisory:\n%s", got)
	}
}
