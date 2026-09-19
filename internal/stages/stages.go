// Package stages holds the body of each harness tool's run() as a callable
// function returning a typed Result, so both the thin cmd/<tool> wrappers and
// cmd/pipeline can invoke it. The three tools (implementation, review,
// retrospective) stay behaviourally identical to their pre-extraction form; the
// pipeline composes them, sharing one config load and one runlog (DESIGN.md
// "The pipeline").
package stages

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/semdedup"
)

var ticketRE = regexp.MustCompile(`^[A-Z]+-\d+$`)

// newSemanticMatcher builds the host-side semantic dedup matcher for filing
// findings (BEH-573), or returns a nil filing.SemanticMatcher when no Anthropic
// API key is available — only a subscription OAuth token, which the x-api-key
// header rejects (BEH-316). filing.File treats a nil matcher as "skip the
// semantic pass", degrading to exact key/title dedup. Returning the interface
// (not the concrete *semdedup.Matcher) keeps the no-key result a true nil
// interface so that nil check fires.
func newSemanticMatcher(cfg config.Config) filing.SemanticMatcher {
	if cfg.AnthropicAPIKey == "" {
		return nil
	}
	return semdedup.New(semdedup.NewAnthropicComplete(cfg.AnthropicAPIKey, cfg.DedupModel))
}

// newUpstream builds the opt-in public-harness-repo sink for harness findings
// (ADR-0011/BEH-640), or nil when feedback.upstream is off (the default) — the
// nil case keeps harness findings in the local artifact dir. github mode binds a
// GitHub adapter to the configured public repo using the host's GH_TOKEN (a
// public repo needs only public_repo scope, and the token attributes the issue to
// the reporting project as provenance). The repo shape is validated at config
// load, so a malformed value never reaches here; a defensive split failure still
// degrades to nil (local sink) rather than filing nowhere.
func newUpstream(cfg config.Config) *filing.Upstream {
	if cfg.Feedback.Upstream != "github" {
		return nil
	}
	owner, repo, err := config.SplitOwnerRepo(cfg.Feedback.Repo)
	if err != nil {
		return nil
	}
	client := github.NewClient(
		github.NewTransport(cfg.GitHubToken), owner, repo,
		github.Options{Findings: cfg.Feedback.FindingsLabel},
	)
	return &filing.Upstream{
		Filer: client, Searcher: client, Recorder: client,
		Container: cfg.Feedback.Repo, Project: cfg.Feedback.Project,
	}
}

// isDiskFull reports whether err is the host-disk-full ENOSPC — surfaced when a
// findings-dir mkdir fails because the disk filled (BEH-540: the BEH-336
// retrospective hard-errored with `mkdir … findings/…: no space left on device`,
// forcing a manual re-run). The disk being full is not the stage's fault and the
// in-sandbox agent can't fix it, so the caller degrades to a clear, actionable
// warning instead of an opaque hard error. errors.Is sees through os.PathError's
// wrapping; other errno values (e.g. EACCES) are not disk-full.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// diskFullWarning builds the actionable warning a stage logs when its findings-dir
// mkdir fails with ENOSPC (BEH-540). It names the stage, surfaces the underlying
// error, and appends sandbox.DiskReclaimHint — the SAME remediation the Preflight
// floor error uses — so the Docker-cache reclaims (the harness's usual disk hog,
// BEH-566) can never drift out of sync between the three disk-full sites.
func diskFullWarning(stage string, err error) string {
	return fmt.Sprintf("%s ⚠ disk full — cannot create findings dir (%s); %s and re-run", stage, err, sandbox.DiskReclaimHint)
}

// hasUpstreamTranscripts reports whether at least one implementation or review
// session transcript exists under a ticket's log dir — the retrospective's
// host-side precondition that an upstream /tdd or /review session actually ran
// (BEH-553). It is the presence half of the gate (the branch is the other half).
// The retrospective's own transcripts (retrospectiveSession) are deliberately not
// matched, so a re-run of a misscheduled retrospective never self-satisfies the
// gate. The glob prefix is the session name, so retry/launch-suffixed transcripts
// (implementation-retry2-<id>.jsonl) still match. Any glob error → false (a
// missing dir means no upstream session ran).
func hasUpstreamTranscripts(logDir string) bool {
	for _, session := range []string{implementationSession, reviewSession} {
		matches, err := filepath.Glob(filepath.Join(logDir, session+"-*.jsonl"))
		if err == nil && len(matches) > 0 {
			return true
		}
	}
	return false
}

// Args is the parsed CLI surface shared by all three tools and the pipeline:
// a ticket identifier plus the universal flags. Force overrides the
// already-merged-on-main dispatch guard (BEH-528) and is only consulted by the
// implementation stage. Next and PreClaimed serve `pipeline --next` (BEH-565):
// Next requests auto-selection (no identifier on the command line), and PreClaimed
// records that selection already claimed the ticket so the implementation stage
// skips its own claim and releases on a preflight failure (ADR-0003).
type Args struct {
	Identifier string
	DryRun     bool
	Verbose    bool
	Force      bool
	// Next requests `pipeline --next` auto-select: resolve the top-of-queue
	// eligible ticket instead of taking an explicit identifier. Pipeline-only.
	Next bool
	// PreClaimed is set when the ticket was already claimed (Todo → In Progress)
	// during selection, so the implementation stage skips MoveToInProgress and
	// releases the claim on a Docker-preflight failure (ADR-0003). False on the
	// hand-passed path, leaving the BEH-316 claim-after-preflight ordering intact.
	PreClaimed bool
}

// ParseArgs parses argv (excluding the program name) into Args. tool names the
// caller for the usage error only — the flag/identifier grammar is identical for
// every tool, which is why it lives here rather than being copied per cmd.
// allowNext gates the pipeline-only `--next` auto-select flag: the three
// standalone tools pass false (they never select a ticket), so for them `--next`
// is an unknown token that falls through to the missing-identifier usage error.
// ErrHelp is returned by ParseArgs when the operator asked for usage rather than
// a run. The cmd wrapper prints Usage to stdout and exits 0: help is a successful
// outcome, and an operator who just unpacked a release archive must be able to
// reach it before holding any credential.
var ErrHelp = errors.New("help requested")

// ErrVersion is returned by ParseArgs when the operator asked which binary this
// is. Like ErrHelp it is a successful outcome, answered before any credential is
// read: a Consumer's `min_harness_version` pin is unactionable if you cannot find
// out what you are running.
var ErrVersion = errors.New("version requested")

// Usage is the one-line grammar for tool. allowNext gates the pipeline-only
// `--next`, so a tool that rejects the flag never advertises it.
func Usage(tool string, allowNext bool) string {
	if allowNext {
		return fmt.Sprintf("usage: %s (<TICKET-ID> | --next) [--dry-run] [--verbose] [--force] [--help]", tool)
	}
	return fmt.Sprintf("usage: %s <TICKET-ID> [--dry-run] [--verbose] [--force] [--help]", tool)
}

func ParseArgs(tool string, argv []string, allowNext bool) (Args, error) {
	// Help outranks the rest of the line: `pipeline BEH-1 --help` explains itself
	// rather than starting a run over BEH-1. Help also outranks --version, so
	// asking for both gets the more informative answer.
	for _, arg := range argv {
		if arg == "--help" || arg == "-h" {
			return Args{}, ErrHelp
		}
	}
	for _, arg := range argv {
		if arg == "--version" {
			return Args{}, ErrVersion
		}
	}

	var a Args
	for _, arg := range argv {
		switch {
		case arg == "--dry-run":
			a.DryRun = true
		case arg == "--verbose":
			a.Verbose = true
		case arg == "--force":
			a.Force = true
		case arg == "--next" && allowNext:
			a.Next = true
		case !strings.HasPrefix(arg, "-") && a.Identifier == "":
			a.Identifier = strings.ToUpper(arg)
		}
	}
	// --next auto-selects, so it carries no identifier — and pairing it with an
	// explicit one is a conflict (name a ticket OR ask for the next, never both).
	if a.Next {
		if a.Identifier != "" {
			return a, fmt.Errorf("%s: --next selects the next ticket — do not also pass an explicit ticket id (%q)", tool, a.Identifier)
		}
		return a, nil
	}
	if !ticketRE.MatchString(a.Identifier) {
		return a, fmt.Errorf("%s  (got: %q)", Usage(tool, allowNext), a.Identifier)
	}
	return a, nil
}

// Result is a stage's typed outcome. It is richer than a bare exit code so the
// pipeline can sequence on OK (and the loop, later, can read structured results
// instead of opaque codes). OK == true implies Err == nil.
type Result struct {
	// OK is the ground-truth verdict: did this stage do its job?
	OK bool
	// Err is a hard setup/IO error (config load, Linear fetch, Docker preflight)
	// that the standalone wrapper prints to stderr before exiting 1. For the
	// pipeline it is simply folded into "not OK".
	Err error
	// Retryable marks a failed outcome that crashed environmentally with nothing
	// to salvage — no worktree, no commit — as opposed to running to completion
	// and producing no handoff diff. Only the former is worth a fresh attempt (a
	// later run, or a healthy host, may get further); the pipeline re-attempts the
	// stage once when it is set (BEH-543). Never set when OK is true.
	Retryable bool
	// ReachedPushedPR marks that this stage pushed the branch and opened a PR. Only
	// the review stage sets it. It is the loop circuit breaker's success signal —
	// "did the ticket ship?" — and is deliberately decoupled from OK: a PR can exist
	// (ReachedPushedPR true) on a not-OK review (CI red after the auto-fix budget),
	// which the breaker must NOT count as a failure (DESIGN.md §Circuit breaker).
	ReachedPushedPR bool
	// SpendingCapAbort marks a stage an external Anthropic spending cap aborted
	// before it could finish its work. The loop treats it as a retry-after-reset
	// control signal, not a ticket failure, so the breaker stays blind to it.
	SpendingCapAbort bool
	// PreflightAbort marks a stage the Docker sandbox preflight refused before any
	// work began — a full host disk or an unreachable daemon, not the ticket's fault.
	// Only the implementation stage sets it (it owns the preflight). Like a cap abort
	// it is an environmental control signal: the loop reclaims disk + backs off and
	// the breaker stays blind to it, so a poison top-of-queue ticket can't rack up
	// identical preflight failures and trip the breaker in seconds.
	PreflightAbort bool
	// SpendingCapResetTime is the exact reset instant the cap-abort message named,
	// resolved at detection (BEH-708). Zero unless this stage cap-aborted with a
	// parseable reset time. The pipeline folds it onto its Outcome so the loop backs
	// off until the cap clears rather than a fixed guess.
	SpendingCapResetTime time.Time
	// RecommendClose marks the BEH-603 no-op disposition: the review stage found the
	// branch makes zero net change against origin/main and declined to open an
	// empty-commit PR, recommending the ticket be closed as a duplicate/superseded.
	// Only the review stage sets it. The loop keeps such a ticket In Progress for a
	// human to close (never released to Todo) and the breaker treats it as neutral.
	RecommendClose bool
}

// LoadConfig loads the harness config: .env first (best-effort), then the
// environment. Shared so the pipeline and each standalone tool load it the same
// way.
func LoadConfig() (config.Config, error) {
	wd, _ := os.Getwd()
	envFile := config.ResolveEnvFile(os.Getenv, wd)
	config.LoadDotEnv(envFile)

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return cfg, err
	}

	// The credential file must not live inside the Consumer checkout: every stage
	// bind-mounts that checkout into its sandbox at its real path (ADR-0002), so a
	// .env there is a file the agent session can read — handing over the tracker and
	// GitHub tokens the harness deliberately keeps host-side. A warning, not an
	// error: the run is already configured and refusing it would strand an operator
	// mid-queue for a file they can move afterwards.
	if config.EnvFileInsideProject(envFile, cfg.ProjectPath) {
		fmt.Fprintf(os.Stderr,
			"warning: credential file %s is inside the bind-mounted checkout %s — every sandbox can read it. Move it to %s.\n",
			envFile, cfg.ProjectPath, config.EnvFileHint(os.Getenv))
	}
	return cfg, nil
}

// Setup performs the once-per-run wiring every tool needs: load config, mint a
// run id, and open the ticket-keyed runlog. The pipeline calls it once and
// passes the results to all three stages (DESIGN.md: one config load, one
// runlog); each standalone tool calls it for itself.
// LogsRoot is the harness logs directory under the primary checkout — where the
// per-ticket log dirs and the global loop.jsonl live. Exposed so the cmd
// entrypoints can wire the same global stream the per-ticket loggers feed.
// Logs are per-Consumer, not per-harness-install, so they live in the Consumer's
// own harness directory (config.ProjectDirName) rather than beside the binary.
func LogsRoot(cfg config.Config) string {
	return filepath.Join(config.ProjectDir(cfg.ProjectPath), "logs")
}

func Setup(identifier string) (config.Config, *runlog.Logger, string, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return config.Config{}, nil, "", err
	}
	runID := runlog.MakeRunID(time.Now())
	log, err := runlog.New(LogsRoot(cfg), identifier)
	if err != nil {
		return cfg, nil, "", err
	}
	return cfg, log, runID, nil
}
