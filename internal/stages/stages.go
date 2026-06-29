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

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/filing"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/semdedup"
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
func ParseArgs(tool string, argv []string, allowNext bool) (Args, error) {
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
		usage := fmt.Sprintf("usage: %s <TICKET-ID> [--dry-run] [--verbose] [--force]", tool)
		if allowNext {
			usage = fmt.Sprintf("usage: %s (<TICKET-ID> | --next) [--dry-run] [--verbose] [--force]", tool)
		}
		return a, fmt.Errorf("%s  (got: %q)", usage, a.Identifier)
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
	config.LoadDotEnv(".env")
	return config.Load(os.Getenv)
}

// Setup performs the once-per-run wiring every tool needs: load config, mint a
// run id, and open the ticket-keyed runlog. The pipeline calls it once and
// passes the results to all three stages (DESIGN.md: one config load, one
// runlog); each standalone tool calls it for itself.
// LogsRoot is the harness logs directory under the primary checkout — where the
// per-ticket log dirs and the global loop.jsonl live. Exposed so the cmd
// entrypoints can wire the same global stream the per-ticket loggers feed.
func LogsRoot(cfg config.Config) string {
	return filepath.Join(cfg.HerdPath, "agent-harness", "logs")
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
