package hostio

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/filing"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/pr"
	"github.com/danoleary/agent-harness/internal/proc"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/semdedup"
	"github.com/danoleary/agent-harness/internal/tracker"
	"github.com/danoleary/agent-harness/internal/trackers"
	"github.com/danoleary/agent-harness/internal/verify"
)

// prCreateTimeout bounds the `gh pr create` network round-trip. Like the git
// remote ops it is a remote call — a stalled network or a blocking gh auth prompt
// would otherwise hang the harness at the very end of a run, stranding a finished,
// already-pushed branch (BEH-386, same hang class as the git fetch/push bound).
const prCreateTimeout = 2 * time.Minute

// ciGhTimeout bounds each individual host-side `gh` call in the CI watch (checks,
// run rerun, run view --log-failed). Generous because `--log-failed` can stream a
// large failed-job log, but still bounded so a stalled gh can't hang the harness
// (same hang class as prCreateTimeout, BEH-386).
const ciGhTimeout = 5 * time.Minute

// Real is the production [Host]: docker through the embedded [Runner], git
// through internal/git, GitHub through `gh`, and the tracker the Consumer
// declared (ADR-0010). It binds the checkout path and the branch prefix once, so
// the (herdPath, branchPrefix, slug) triple never has to be threaded again.
type Real struct {
	*Runner
	cfg config.Config
	log *runlog.Logger

	// client is the lazily-resolved tracker adapter, cached so a stage that fetches
	// the ticket and later files findings builds one client, not three.
	client    tracker.Tracker
	clientErr error
	resolved  bool
}

// New builds the production Host for one run of one ticket.
func New(cfg config.Config, log *runlog.Logger, runID string, verbose bool) *Real {
	return &Real{Runner: NewRunner(cfg, log, runID, verbose), cfg: cfg, log: log}
}

// compile-time proof that the production adapter really does satisfy the port.
var _ Host = (*Real)(nil)

// --- tracker + findings -----------------------------------------------------

// Tracker resolves the Consumer-declared tracker adapter once and caches it (and
// its error) for the rest of the run.
func (h *Real) Tracker() (tracker.Tracker, error) {
	if !h.resolved {
		h.client, h.clientErr = trackers.New(h.cfg.Tracker, trackers.Secrets{
			LinearKey:   h.cfg.LinearAPIKey,
			GitHubToken: h.cfg.GitHubToken,
			JiraBaseURL: h.cfg.JiraBaseURL,
			JiraEmail:   h.cfg.JiraEmail,
			JiraToken:   h.cfg.JiraAPIToken,
		})
		h.resolved = true
	}
	return h.client, h.clientErr
}

// AlreadyFiled lists the finding classes a prior run already filed, so a re-run's
// session treats them as settled instead of re-deriving them (BEH-539).
func (h *Real) AlreadyFiled(findingsDir, teamID string) []filing.PriorFinding {
	client, err := h.Tracker()
	if err != nil {
		return nil
	}
	return filing.AlreadyFiled(findingsDir, teamID, client, h.log)
}

// FileFindings files everything in the dropbox to the Consumer's tracker. This is
// the plain, audience-blind sink the implementation stage uses: its dropbox
// protocol asks for harness/environment friction only and is not taught to
// classify, so it must not go through the audience router.
func (h *Real) FileFindings(findingsDir, teamID, key string) {
	client, err := h.Tracker()
	if err != nil {
		return
	}
	filing.File(findingsDir, teamID, key, client, client, h.matcher(), client, h.log)
}

// RouteFindings routes the dropbox by audience (ADR-0011): project findings to
// the Consumer's tracker, harness findings to the local artifact dir or — when
// the Consumer opted in — to the public harness repo.
func (h *Real) RouteFindings(findingsDir, teamID, key string) {
	client, err := h.Tracker()
	if err != nil {
		return
	}
	filing.Route(findingsDir, filing.HarnessFindingsDir(h.cfg.ProjectPath), teamID, key,
		client, client, h.matcher(), client, h.upstream(), h.log)
}

// matcher builds the host-side semantic dedup matcher for filing findings
// (BEH-573), or returns a nil filing.SemanticMatcher when no Anthropic API key is
// available — only a subscription OAuth token, which the x-api-key header rejects
// (BEH-316). filing.File treats a nil matcher as "skip the semantic pass",
// degrading to exact key/title dedup. Returning the interface (not the concrete
// *semdedup.Matcher) keeps the no-key result a true nil interface so that nil
// check fires.
func (h *Real) matcher() filing.SemanticMatcher {
	if h.cfg.AnthropicAPIKey == "" {
		return nil
	}
	return semdedup.New(semdedup.NewAnthropicComplete(h.cfg.AnthropicAPIKey, h.cfg.DedupModel))
}

// upstream builds the opt-in public-harness-repo sink for harness findings
// (ADR-0011/BEH-640), or nil when feedback.upstream is off (the default) — the
// nil case keeps harness findings in the local artifact dir. github mode binds a
// GitHub adapter to the configured public repo using the host's GH_TOKEN (a
// public repo needs only public_repo scope, and the token attributes the issue to
// the reporting project as provenance). The repo shape is validated at config
// load, so a malformed value never reaches here; a defensive split failure still
// degrades to nil (local sink) rather than filing nowhere.
func (h *Real) upstream() *filing.Upstream {
	if h.cfg.Feedback.Upstream != "github" {
		return nil
	}
	owner, repo, err := config.SplitOwnerRepo(h.cfg.Feedback.Repo)
	if err != nil {
		return nil
	}
	client := github.NewClient(
		github.NewTransport(h.cfg.GitHubToken), owner, repo,
		github.Options{Findings: h.cfg.Feedback.FindingsLabel},
	)
	return &filing.Upstream{
		Filer: client, Searcher: client, Recorder: client,
		Container: h.cfg.Feedback.Repo, Project: h.cfg.Feedback.Project,
	}
}

// --- repo -------------------------------------------------------------------

func (h *Real) WorktreePath(slug string) string { return gitpkg.WorktreePath(h.cfg.ProjectPath, slug) }
func (h *Real) BranchName(slug string) string   { return gitpkg.BranchName(h.cfg.BranchPrefix, slug) }

// WorktreeExists reports whether the ticket's worktree is on disk — the
// implementation stage's "already provisioned?" check and the review stage's
// precondition ("run `implementation <ticket>` first").
func (h *Real) WorktreeExists(slug string) bool {
	_, err := os.Stat(h.WorktreePath(slug))
	return err == nil
}

func (h *Real) CreateWorktree(slug string) error {
	return gitpkg.CreateWorktree(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

func (h *Real) RemoveWorktree(slug string) error {
	return gitpkg.RemoveWorktree(h.cfg.ProjectPath, slug)
}

func (h *Real) WorktreeClean(slug string) bool { return gitpkg.WorktreeClean(h.WorktreePath(slug)) }

// StripHandoffPaths removes the Consumer-declared build artifacts the sandbox
// produced before the worktree goes to a (possibly non-Linux) reviewer, so their
// platform-specific contents don't crash the reviewer's gates (BEH-412/641).
func (h *Real) StripHandoffPaths(slug string) error {
	return gitpkg.StripWorktreePaths(h.WorktreePath(slug), h.cfg.HandoffStripPaths)
}

func (h *Real) Checkpoint(slug, key, stage string) error {
	return gitpkg.CheckpointCommit(h.WorktreePath(slug), key, stage)
}

func (h *Real) HeadSHA(slug string) (string, error) { return gitpkg.HeadSHA(h.WorktreePath(slug)) }

func (h *Real) EnsureCIRerunCommit(slug, headBefore string) error {
	return gitpkg.EnsureCIRerunCommit(h.WorktreePath(slug), headBefore)
}

func (h *Real) GroundTruth(slug string) verify.GroundTruth {
	return gitpkg.GatherTddGroundTruth(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

func (h *Real) BranchDiffEmpty(slug string) bool {
	return gitpkg.BranchDiffEmpty(h.WorktreePath(slug))
}

func (h *Real) BranchDocsOnly(slug string) bool {
	return gitpkg.BranchDocsOnly(h.WorktreePath(slug), h.cfg.DocsOnlyExcludedRoots)
}

func (h *Real) BranchExists(slug string) bool {
	return gitpkg.BranchExists(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

func (h *Real) BranchPushed(slug string) bool {
	return gitpkg.BranchPushed(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

func (h *Real) CommitSubjects(slug string) []string {
	return gitpkg.CommitSubjects(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

func (h *Real) Rebase(slug string) RebaseResult {
	return gitpkg.RebaseOntoMain(h.WorktreePath(slug))
}

func (h *Real) AbortRebase(slug string) { gitpkg.AbortRebase(h.WorktreePath(slug)) }

func (h *Real) IsDisjoint(slug string) bool {
	return gitpkg.IsDisjointFrom(h.WorktreePath(slug), baseRef)
}

func (h *Real) IsRebased(slug string) bool {
	return gitpkg.IsRebasedOnto(h.WorktreePath(slug), baseRef)
}

func (h *Real) Regraft(slug string) error {
	return gitpkg.RegraftOntoBase(h.WorktreePath(slug), baseRef)
}

func (h *Real) TicketAlreadyOnMain(key string) bool {
	return gitpkg.TicketAlreadyOnMain(h.cfg.ProjectPath, key)
}

// ResolvedAdvisory warns when a ticket cites code symbols that no longer exist
// under the Consumer's declared source roots (BEH-544). A Consumer that declares
// no `source_roots` gets no advisory rather than a scan of a guessed path
// (BEH-641).
func (h *Real) ResolvedAdvisory(key, description string) string {
	return gitpkg.ResolvedAdvisory(h.sourceRoots(), key, description)
}

func (h *Real) ResumedBranchAdvisory(slug, key string) string {
	return gitpkg.ResumedBranchAdvisory(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug, key)
}

// sourceRoots resolves the Consumer's declared source roots (checkout-relative,
// e.g. "src" or "web/src") to absolute paths. An empty declaration yields no
// roots, which disables the advisory rather than scanning a guessed directory.
func (h *Real) sourceRoots() []string {
	if len(h.cfg.SourceRoots) == 0 {
		return nil
	}
	roots := make([]string, 0, len(h.cfg.SourceRoots))
	for _, r := range h.cfg.SourceRoots {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, filepath.Join(h.cfg.ProjectPath, r))
		}
	}
	return roots
}

// --- remote -----------------------------------------------------------------

func (h *Real) FetchMain() error { return gitpkg.FetchMain(h.cfg.ProjectPath) }

func (h *Real) Push(slug string) error {
	return gitpkg.Push(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

func (h *Real) PushForceWithLease(slug string) error {
	return gitpkg.PushForceWithLease(h.cfg.ProjectPath, h.cfg.BranchPrefix, slug)
}

// CreatePR opens the pull request from the main checkout with `gh`, which infers
// the origin repo from the checkout. GH_TOKEN stays host-only (ADR-0002) — gh
// reads it from the harness env. Returns the created PR URL (gh prints it to
// stdout).
func (h *Real) CreatePR(slug, title, body string) (string, error) {
	out, err := proc.CombinedOutputInDir(
		prCreateTimeout, h.cfg.ProjectPath,
		"gh", "pr", "create",
		"--head", h.BranchName(slug),
		"--base", baseBranch,
		"--title", title,
		"--body", body,
	)
	if err != nil {
		return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *Real) PRExists(slug string) bool { return pr.Exists(h.cfg.ProjectPath, h.BranchName(slug)) }

// OpenPRExists narrows PRExists to a PR still in flight. The committed-fix
// recovery asks it rather than PRExists: a branch whose PR was merged or closed
// and then re-opened as new work is still strandable, so only an OPEN PR means
// "this branch already escaped the worktree".
func (h *Real) OpenPRExists(slug string) bool {
	return pr.OpenExists(h.cfg.ProjectPath, h.BranchName(slug))
}

// ChecksReadable reports whether GH_TOKEN can read check runs — the post-PR
// CI-watch needs a classic repo-scoped PAT (fine-grained PATs lack the Checks
// permission). Not fatal: the PR still ships and the watch degrades gracefully
// (BEH-476).
func (h *Real) ChecksReadable() (bool, string) {
	return ci.ChecksReadable(func(name string, args ...string) ([]byte, error) {
		return proc.CombinedOutputInDir(ciGhTimeout, h.cfg.ProjectPath, name, args...)
	})
}

// WatchCI polls the PR head's checks and, on a real failure, runs w.Fix over the
// worktree, pushes, and re-polls — bounded by attempts + wall clock. Every gh
// call, the push and the reactive rebase are host-side (ADR-0002); only the
// diagnose-and-fix happens in the sandbox, which is why Fix is the one thing the
// caller supplies.
func (h *Real) WatchCI(w CIWatch) ci.Outcome {
	cfg := ci.Config{
		MaxFixAttempts: h.cfg.CIMaxFixAttempts,
		Budget:         h.cfg.CIFixBudget,
		PollInterval:   h.cfg.CIPollInterval,
		PollBudget:     h.cfg.CIPollBudget,
		PollStall:      h.cfg.CIPollStall,
		PollMaxBudget:  h.cfg.CIPollMaxBudget,
	}
	driver := ci.NewGhDriver(
		h.cfg.ProjectPath, h.BranchName(w.Slug), cfg, ciGhTimeout,
		w.Fix,
		func() error { return h.Push(w.Slug) },
		func() (ci.RebaseVerdict, error) { return h.rebaseOntoBase(w.Slug) },
		func() bool { return h.BranchDiffEmpty(w.Slug) },
		func() bool { return h.BranchDocsOnly(w.Slug) },
	)
	return ci.WatchAndFix(driver, cfg, time.Now)
}

// rebaseOntoBase is the reactive auto-rebase the CI watch invokes when a green PR
// reads CONFLICTING against base (main moved underneath it after the push). It
// refreshes origin/main — the conflict means main advanced since the pre-push
// rebase, so we must replay onto the truly-latest base — rebases the worktree, and
// on a clean replay force-with-lease re-pushes the rewritten branch. A genuine
// content conflict returns RebaseConflict (branch left untouched) for a human; any
// fetch/push failure is surfaced as an error the watch reports. Keeping it here is
// what lets internal/ci stay free of internal/git (BEH-570).
func (h *Real) rebaseOntoBase(slug string) (ci.RebaseVerdict, error) {
	if err := h.FetchMain(); err != nil {
		return ci.RebaseConflict, fmt.Errorf("fetch %s before rebase: %w", baseRef, err)
	}
	if h.Rebase(slug) == gitpkg.RebaseConflict {
		return ci.RebaseConflict, nil
	}
	if err := h.PushForceWithLease(slug); err != nil {
		return ci.RebaseClean, fmt.Errorf("force-with-lease re-push after rebase: %w", err)
	}
	return ci.RebaseClean, nil
}
