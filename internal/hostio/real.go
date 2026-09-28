package hostio

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/filing"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/pr"
	"github.com/danoleary/agent-harness/internal/proc"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/semdedup"
	"github.com/danoleary/agent-harness/internal/ship"
	"github.com/danoleary/agent-harness/internal/tracker"
	"github.com/danoleary/agent-harness/internal/trackers"
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

// router builds the findings router for this run: the Consumer's tracker as the
// project sink, the ADR-0011 upstream sink (or nil for the local artifact dir),
// the semantic matcher, and the run's narration log. ok is false when the
// tracker could not be resolved — with no sink there is nothing to file to, and
// a findings step is best-effort (ADR-0001), so the caller silently does nothing.
func (h *Real) router() (filing.Router, bool) {
	client, err := h.Tracker()
	if err != nil {
		return filing.Router{}, false
	}
	return filing.Router{
		Project:    client,
		Upstream:   h.upstream(),
		Matcher:    h.matcher(),
		HarnessDir: filing.HarnessFindingsDir(h.cfg.ProjectPath),
		Log:        h.log,
	}, true
}

// AlreadyFiled lists the finding classes a prior run already filed, so a re-run's
// session treats them as settled instead of re-deriving them (BEH-539).
func (h *Real) AlreadyFiled(findingsDir, teamID string) []filing.PriorFinding {
	r, ok := h.router()
	if !ok {
		return nil
	}
	return r.AlreadyFiled(findingsDir, teamID)
}

// FileFindings files everything in the dropbox to the Consumer's tracker. This is
// the plain, audience-blind sink the implementation stage uses: its dropbox
// protocol asks for harness/environment friction only and is not taught to
// classify, so it must not go through the audience router.
func (h *Real) FileFindings(findingsDir, teamID, key string) {
	if r, ok := h.router(); ok {
		r.File(findingsDir, teamID, key)
	}
}

// RouteFindings routes the dropbox by audience (ADR-0011): project findings to
// the Consumer's tracker, harness findings to the local artifact dir or — when
// the Consumer opted in — to the public harness repo.
func (h *Real) RouteFindings(findingsDir, teamID, key string) {
	if r, ok := h.router(); ok {
		r.Route(findingsDir, teamID, key)
	}
}

// matcher builds the host-side semantic dedup matcher for filing findings
// (BEH-573), or returns a nil filing.SemanticMatcher when no Anthropic API key is
// available — only a subscription OAuth token, which the x-api-key header rejects
// (BEH-316). filing.Router treats a nil matcher as "skip the semantic pass",
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
// the reporting project as provenance). It goes through trackers.New like every
// other adapter, so the upstream sink is a [tracker.Tracker] rather than a
// hand-built client and nothing here has to import a concrete adapter. The repo
// shape is validated at config load and again by trackers.New, so a malformed
// value never reaches here; a defensive failure still degrades to nil (local
// sink) rather than filing nowhere.
func (h *Real) upstream() *filing.Upstream {
	if h.cfg.Feedback.Upstream != "github" {
		return nil
	}
	sink, err := trackers.New(
		config.TrackerConfig{
			Kind: "github",
			Repo: h.cfg.Feedback.Repo,
			// The GitHub adapter reads findings_label_id as a label NAME, which is
			// exactly what feedback.findings_label carries.
			FindingsLabelID: h.cfg.Feedback.FindingsLabel,
		},
		trackers.Secrets{GitHubToken: h.cfg.GitHubToken},
	)
	if err != nil {
		return nil
	}
	return &filing.Upstream{
		Sink:      sink,
		Container: h.cfg.Feedback.Repo,
		Project:   h.cfg.Feedback.Project,
	}
}

// --- repo -------------------------------------------------------------------

// checkout binds the Consumer's checkout path and branch prefix to internal/git
// once. Every Repo/Remote method below names a ticket slug and nothing else — the
// (herdPath, branchPrefix, slug) triple that used to travel to every git call is
// now the [git.Worktree] value wt returns.
func (h *Real) checkout() gitpkg.Checkout {
	return gitpkg.Open(h.cfg.ProjectPath, h.cfg.BranchPrefix)
}

// wt is the ticket's worktree: the harness's central noun, minted on demand.
func (h *Real) wt(slug string) gitpkg.Worktree { return h.checkout().Worktree(slug) }

func (h *Real) WorktreePath(slug string) string { return h.wt(slug).Path() }
func (h *Real) BranchName(slug string) string   { return h.wt(slug).Branch() }

// WorktreeExists reports whether the ticket's worktree is on disk — the
// implementation stage's "already provisioned?" check and the review stage's
// precondition ("run `implementation <ticket>` first").
func (h *Real) WorktreeExists(slug string) bool { return h.wt(slug).Exists() }

func (h *Real) CreateWorktree(slug string) error { return h.wt(slug).Create() }

func (h *Real) RemoveWorktree(slug string) error { return h.wt(slug).Remove() }

func (h *Real) WorktreeClean(slug string) bool { return h.wt(slug).Clean() }

// StripHandoffPaths removes the Consumer-declared build artifacts the sandbox
// produced before the worktree goes to a (possibly non-Linux) reviewer, so their
// platform-specific contents don't crash the reviewer's gates (BEH-412/641).
func (h *Real) StripHandoffPaths(slug string) error {
	return h.wt(slug).StripPaths(h.cfg.HandoffStripPaths)
}

func (h *Real) Checkpoint(slug, key, stage string) error {
	return h.wt(slug).Checkpoint(key, stage)
}

func (h *Real) HeadSHA(slug string) (string, error) { return h.wt(slug).HeadSHA() }

func (h *Real) EnsureCIRerunCommit(slug, headBefore string) error {
	return h.wt(slug).EnsureCIRerunCommit(headBefore)
}

func (h *Real) CommitsAhead(slug string) int { return h.wt(slug).CommitsAhead() }

func (h *Real) BranchDisjoint(slug string) bool { return h.wt(slug).DisjointHistory() }

func (h *Real) BranchDiffEmpty(slug string) bool { return h.wt(slug).DiffEmpty() }

func (h *Real) BranchDocsOnly(slug string) bool {
	return h.wt(slug).DocsOnly(h.cfg.DocsOnlyExcludedRoots)
}

func (h *Real) BranchExists(slug string) bool { return h.wt(slug).BranchExists() }

func (h *Real) BranchPushed(slug string) bool { return h.wt(slug).BranchPushed() }

func (h *Real) CommitSubjects(slug string) []string { return h.wt(slug).CommitSubjects() }

func (h *Real) Rebase(slug string) RebaseResult { return h.wt(slug).Rebase() }

func (h *Real) AbortRebase(slug string) { h.wt(slug).AbortRebase() }

func (h *Real) IsDisjoint(slug string) bool { return h.wt(slug).IsDisjointFrom(baseRef) }

func (h *Real) IsRebased(slug string) bool { return h.wt(slug).IsRebasedOnto(baseRef) }

func (h *Real) Regraft(slug string) error { return h.wt(slug).RegraftOntoBase(baseRef) }

func (h *Real) TicketAlreadyOnMain(key string) bool {
	return h.checkout().TicketAlreadyOnMain(key)
}

// ResolvedAdvisory warns when a ticket cites code symbols that no longer exist
// under the Consumer's declared source roots (BEH-544). A Consumer that declares
// no `source_roots` gets no advisory rather than a scan of a guessed path
// (BEH-641).
func (h *Real) ResolvedAdvisory(key, description string) string {
	return gitpkg.ResolvedAdvisory(h.sourceRoots(), key, description)
}

func (h *Real) ResumedBranchAdvisory(slug, key string) string {
	return h.wt(slug).ResumedBranchAdvisory(key)
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

func (h *Real) FetchMain() error { return h.checkout().FetchMain() }

func (h *Real) Push(slug string) error { return h.wt(slug).Push() }

func (h *Real) PushForceWithLease(slug string) error { return h.wt(slug).PushForceWithLease() }

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
// rebase, so we must replay onto the truly-latest base — replays the worktree
// through the same [ship.Replay] the pre-push landing uses (so the disjoint-history
// guard is shared), and on a clean replay force-with-lease re-pushes the rewritten
// branch. A content conflict or a disjoint history returns RebaseConflict with the
// branch left on its original tip, for a human; any fetch/push failure is surfaced
// as an error the watch reports. Unlike a landing, a failed fetch stops here: a
// branch replayed onto a base we could not refresh would still read CONFLICTING.
// Keeping it here is what lets internal/ci stay free of internal/git (BEH-570).
func (h *Real) rebaseOntoBase(slug string) (ci.RebaseVerdict, error) {
	if err := h.FetchMain(); err != nil {
		return ci.RebaseConflict, fmt.Errorf("fetch %s before rebase: %w", baseRef, err)
	}
	if ship.Replay(h, slug) != ship.ReplayClean {
		return ci.RebaseConflict, nil
	}
	if err := h.PushForceWithLease(slug); err != nil {
		return ci.RebaseClean, fmt.Errorf("force-with-lease re-push after rebase: %w", err)
	}
	return ci.RebaseClean, nil
}
