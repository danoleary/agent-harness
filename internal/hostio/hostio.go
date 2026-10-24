// Package hostio is the harness's host-side I/O seam: the one interface a Stage
// talks to when it needs Docker, git, gh or the tracker (ADR-0002 — the host owns
// every remote operation; the sandbox only ever runs `claude`).
//
// Before this package the three Stage bodies reached straight for `gitpkg.*`,
// `sandbox.*`, `session.Run`, `trackers.New`, `pr.*` and `ci.*` at package level,
// so none of the ~2,700 lines of harness policy they hold could be reached from a
// test: the only way in was a real Docker daemon and a real tracker. Naming that
// I/O as one substitutable port makes the policy testable without changing it.
//
// Two adapters ship, which is what makes this a real seam rather than a
// hypothetical one:
//
//   - [Real] — docker / git / gh / the configured tracker.
//   - [Fake] — scripted outcomes and recorded calls, for tests.
//
// The sandbox half is fronted by a [Runner], which owns {cfg, log, runID, prefix}
// and mints a container's `--name` together with the argv that carries it. That
// turns the harness's sharpest unenforced invariant — "Options.ContainerName must
// equal the `--name` in the argv or the timeout `docker kill` misses its target" —
// from a comment repeated at four call sites into something a caller cannot get
// wrong, because a caller no longer names containers at all.
package hostio

import (
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/filing"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ship"
	"github.com/danoleary/agent-harness/internal/tracker"
	"github.com/danoleary/agent-harness/internal/verify"
)

// AgentRun describes one sandboxed claude session. Label is the *role* of the
// run ("implementation", "review", "cifix-1"); the Runner derives the container
// name and the transcript filename from it, so the two can never disagree and a
// caller never mints either. An empty FindingsDir mounts no dropbox — the review
// session emits no findings, and having nowhere to write keeps its "do not write
// findings" steering honest. Model is the claude `--model`; the stage picks it,
// so each stage can run on a different model.
type AgentRun struct {
	Label       string
	Model       string
	Prompt      string
	FindingsDir string
	Cap         time.Duration
	Retry       Retry
}

// ShellRun describes one secret-free throwaway container that runs a shell
// command in a worktree — the Consumer's post_create hook, or one declared gate.
// It carries no credential at all (it runs no model), so nothing crosses the
// sandbox boundary for it.
type ShellRun struct {
	Label        string
	Command      string
	WorktreePath string
	Cap          time.Duration
	Retry        Retry
}

// Retry is the transient-failure retry schedule for one launch: re-run while the
// outcome is environmental (the 137 OOM-kill of BEH-524, or a transient exit-125
// launch failure — BEH-542/550) and attempts remain. The zero value runs exactly
// once, which is what a session with its own higher-level retry wants.
//
// Suffix names the per-attempt label suffix ("retry" → `<label>-retry2`), so the
// implementation stage's launch retries stay distinguishable from its
// usage-policy-refusal retries in both container names and transcripts. Empty
// means "retry".
//
// Notify is called immediately before attempt N (N >= 2) with the backoff already
// slept, so the caller narrates the retry in its own voice without owning the
// mechanism.
type Retry struct {
	MaxAttempts int
	Backoff     func(attempt int) time.Duration
	Suffix      string
	Notify      func(attempt int, waited time.Duration)
}

// Result is a finished container run: the session outcome plus the names the
// Runner minted for it. Callers narrate Transcript rather than re-deriving it —
// they no longer can, which is the point.
type Result struct {
	session.Outcome
	// Container is the `--name` the final attempt ran under.
	Container string
	// Transcript is the filename (under the ticket's log dir) the run was teed to.
	Transcript string
	// Attempts is how many launches actually happened (1 when nothing retried).
	Attempts int
}

// CIWatch parameterises the post-PR CI watch. The host owns the whole gh side —
// polling checks, re-running them, fetching failed logs, pushing, and the
// reactive rebase — so a Stage supplies only Fix, the sandboxed diagnose-and-fix
// session it wants run against a red check. That collapses ci.NewGhDriver's nine
// positional parameters at the call site to one field.
type CIWatch struct {
	Slug string
	Fix  func(ciLogs string, logAvailable bool) error
}

// RebaseResult, RebaseClean and RebaseConflict re-export git's rebase verdict so
// a Stage can branch on a pre-push conflict without importing internal/git for a
// single constant.
type RebaseResult = gitpkg.RebaseResult

const (
	RebaseClean    = gitpkg.RebaseClean
	RebaseConflict = gitpkg.RebaseConflict
)

// Sandbox is the Docker-facing half of [Host]: the launch preflight, the two
// kinds of container the harness runs, and the argv previews `--dry-run` prints.
// The previews come from the same code path as the launches, so a dry-run can
// never show a command the harness would not actually run.
type Sandbox interface {
	Preflight() error
	Agent(AgentRun) Result
	Shell(ShellRun) Result
	AgentPreview(AgentRun) []string
	ShellPreview(ShellRun) []string
}

// Planner is the slice of [Host] a dry-run plan reads: where a ticket's worktree
// and branch live, and the argv each run would launch with.
type Planner interface {
	WorktreePath(slug string) string
	BranchName(slug string) string
	AgentPreview(AgentRun) []string
	ShellPreview(ShellRun) []string
}

// Repo is the git-facing half: everything that reads or mutates the Consumer
// checkout and one ticket's worktree. Every method is keyed by the ticket slug —
// the adapter holds the checkout path and the branch prefix, so a Stage names the
// ticket and nothing else. Below the adapter the slug becomes a [git.Worktree],
// the value that carries the checkout, the branch and the worktree path together.
type Repo interface {
	WorktreePath(slug string) string
	BranchName(slug string) string
	WorktreeExists(slug string) bool
	CreateWorktree(slug string) error
	RemoveWorktree(slug string) error
	WorktreeClean(slug string) bool
	StripHandoffPaths(slug string) error
	Checkpoint(slug, key, stage string) error
	HeadSHA(slug string) (string, error)
	EnsureCIRerunCommit(slug, headBefore string) error
	CommitsAhead(slug string) int
	BranchDisjoint(slug string) bool
	BranchDiffEmpty(slug string) bool
	BranchDocsOnly(slug string) bool
	BranchExists(slug string) bool
	BranchPushed(slug string) bool
	CommitSubjects(slug string) []string
	Rebase(slug string) RebaseResult
	AbortRebase(slug string)
	IsDisjoint(slug string) bool
	IsRebased(slug string) bool
	Regraft(slug string) error
	TicketAlreadyOnMain(key string) bool
	ResolvedAdvisory(key, description string) string
	ResumedBranchAdvisory(slug, key string) string
}

// Remote is the network-facing half: the origin fetch, the two push shapes, and
// everything that speaks to GitHub through `gh`.
type Remote interface {
	FetchMain() error
	Push(slug string) error
	PushForceWithLease(slug string) error
	CreatePR(slug, title, body string) (string, error)
	PRExists(slug string) bool
	OpenPRExists(slug string) bool
	ChecksReadable() (bool, string)
	WatchCI(CIWatch) ci.Outcome
}

// Findings is the tracker port plus the three findings-dropbox operations that
// reach it. Keeping them here is what lets internal/stages stop constructing a
// tracker adapter of its own for the ADR-0011 upstream sink; the adapter itself
// is selected in one place (internal/trackers), so neither the stage layer nor
// this one names a concrete tracker.
type Findings interface {
	Tracker() (tracker.Tracker, error)
	AlreadyFiled(findingsDir, teamID string) []filing.PriorFinding
	FileFindings(findingsDir, teamID, key string)
	RouteFindings(findingsDir, teamID, key string)
}

// Host is the whole host-side surface one Stage needs. [Real] and [Fake]
// implement it.
//
// It also satisfies [verify.GroundTruth] — the git-read subset the harness's
// "did this Stage do its job?" decisions gather for themselves — and [ship.Port],
// the slice that lands a branch, so a Stage hands either module the Host it already
// holds instead of re-packing the calls into a parameter struct. The assertions
// below keep them in step: adding a method to either port without adding it here
// stops compiling.
type Host interface {
	Sandbox
	Repo
	Remote
	Findings
	// RunID is the id of the run this Host is bound to — the same id the Runner
	// stamps into every container name and transcript. Stages narrate it.
	RunID() string
}

var (
	_ verify.GroundTruth = (Host)(nil)
	_ ship.Port          = (Host)(nil)
)
