// Package config resolves harness configuration for a single tool invocation
// (implementation, review, retrospective).
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/version"
)

// Config is the resolved harness configuration, in the two halves ADR-0008
// separates: "Project configuration lives in the Consumer repo … only secrets and
// host paths stay as env vars on the harness side." Host is the harness side's
// half and Project the Consumer's, so the line is drawn by the type rather than
// only in prose. Both are embedded, so cfg.Gates and cfg.GitHubToken read as
// before; a caller that needs one half takes cfg.Host or cfg.Project.
type Config struct {
	Host
	Project
}

// Host is what the harness side supplies through its environment: the secrets,
// the host paths, and the tuning knobs. None of it is read from the Consumer's
// checkout.
type Host struct {
	// Secrets. Each is host-only (ADR-0001/0002): none crosses into the sandbox.

	// LinearAPIKey is the host-only Linear key (ADR-0001) — never passed into the sandbox.
	LinearAPIKey string
	// GitHubToken is the host-only GH_TOKEN (ADR-0001) — used for the harness's own
	// push + `gh pr create` and, when tracker.kind=github, the GitHub Issues tracker
	// adapter. Held host-side like LinearAPIKey; never passed into the sandbox.
	GitHubToken string
	// JiraBaseURL / JiraEmail / JiraAPIToken are the host-only Jira Cloud Basic-auth
	// triple (ADR-0001), read only when tracker.kind=jira: the site URL, the account
	// email, and its API token. Held host-side; never passed into the sandbox. They
	// are optional at Load (a non-jira consumer sets none) — trackers.New fails loud
	// if kind=jira but any is missing.
	JiraBaseURL  string
	JiraEmail    string
	JiraAPIToken string
	// AnthropicAPIKey is the host-only API key used for the cheap host-side semantic
	// dedup model call when filing findings (BEH-573). Unlike the sandbox credential
	// (which is validated for presence but never stored — it crosses into the
	// container via docker `-e`, ADR-0002), this is held host-side for the harness's
	// own model call. It is "" when only a subscription OAuth token is set — the
	// x-api-key header rejects an OAuth token (BEH-316) — and semantic dedup is then
	// skipped (best-effort), with filing degrading to exact key/title dedup.
	AnthropicAPIKey string

	// Host paths.

	// ProjectPath is the absolute host path to the Consumer checkout to bind-mount.
	ProjectPath string
	// StopFile is the STOP sentinel path used by startup-clear and the stop check. A
	// relative path is resolved against ProjectPath by cmd/loop.
	StopFile string

	// Tuning knobs, each one row of Host.knobs.

	// TddTimeout is the hard cap for the tdd (implementation) session, enforced on
	// ACTIVE (monotonic) in-sandbox time — host sleep is excluded (BEH-608), and it is
	// NOT the total wall-clock the session's claude `duration_ms` reports (that also
	// carries model/API latency and any host sleep, so it routinely reads a multiple
	// of this cap without the cap ever being blown — BEH-688).
	TddTimeout time.Duration
	// TddLargeRefactorTimeout is the larger active-time cap granted to a tdd session
	// whose ticket is a multi-file "extract-and-rewire" refactor (ticket.IsLargeRefactor).
	// Such tickets are inherently sequential — extract N shared modules, then rewire N
	// call sites to consume them — and routinely overran the 30m TddTimeout mid-surgery,
	// leaving an uncompilable half-rewired checkpoint that had to be redone from scratch
	// (BEH-441, BEH-688 Symptom 2). Kept strictly above TddTimeout.
	TddLargeRefactorTimeout time.Duration
	// ReviewTimeout is the active-time (monotonic, host-sleep-excluded — BEH-608) cap
	// for every review-family session (the prep
	// install, the qualitative review, the host-side gate re-run, and each CI-fix
	// cycle). It is kept above SessionIdleTimeout so a stalled review session is
	// reaped by the idle/no-progress watchdog before this hard cap, not at it; a
	// 15m cap once sat below the 20m idle window, so the idle watchdog was inert for
	// the whole family and memory-pressured sessions burned to the cap (BEH-535/538).
	ReviewTimeout time.Duration
	// RetrospectiveTimeout is the active-time (monotonic) cap for the retrospective
	// session. It is deliberately larger than the tdd cap (which it used to borrow): the
	// retrospective is a read-heavy step that parses several large jsonl
	// transcripts, and borrowing the 30m tdd cap killed it mid-read before it
	// could write findings (BEH-536).
	RetrospectiveTimeout time.Duration
	// SessionIdleTimeout is the heartbeat window passed to every sandboxed session:
	// if the docker stream produces no output for this long the stream is treated as
	// dead (e.g. an API connection silently severed while the host slept) and the
	// container is killed. Distinct from the per-session hard caps above, which bound
	// total runtime; this bounds silence. It must stay comfortably ABOVE the slowest
	// single silent in-sandbox command: the agent's stream emits nothing between a
	// tool_use and its tool_result, so one long quiet tool call (a `pnpm run build`
	// or `test-storybook` run) is legitimately silent for minutes — set too low it
	// reaps a healthy session mid-build. The active-time hard cap is the backstop, so
	// this only needs to detect a dead stream faster than the cap, not race it.
	SessionIdleTimeout time.Duration
	// ImplementationModel, ReviewModel and RetrospectiveModel are the claude
	// `--model` each stage's sessions run on. ReviewModel also covers the review
	// stage's cifix and rebasefix sessions. Each defaults to an exact Opus snapshot,
	// not the floating `opus` alias: the CLI's own default is not guaranteed to be
	// Opus and a past run silently fell back to Sonnet (BEH-316), while the alias
	// once resolved to a stale Opus 4.1 prone to a false-positive usage-policy
	// refusal on long sessions (BEH-389).
	ImplementationModel string
	ReviewModel         string
	RetrospectiveModel  string
	// CIMaxFixAttempts caps how many diagnose+fix+push cycles the review tool runs
	// against a red CI before giving up and leaving the PR for a human (BEH-414).
	CIMaxFixAttempts int
	// CIFixBudget is the overall wall-clock cap on the post-PR CI watch+fix loop.
	CIFixBudget time.Duration
	// CIPollInterval is how often CI checks are re-polled while still pending.
	CIPollInterval time.Duration
	// CIPollBudget caps a single wait for CI checks to reach a terminal state.
	CIPollBudget time.Duration
	// CIPollStall is the no-progress window: once a pending check set that carries stall
	// evidence (a settled real gate or an EXPECTED context) stops changing for this long,
	// the poll gives up early (ErrPollStalled) rather than burning the full CIPollBudget on
	// a check wedged pending — the merge-queue / main-only context that GitHub reports as
	// expected-but-never-run on a PR. A slow all-pending cold start carries no such evidence
	// and is never stalled; it rides to CIPollBudget (BEH-620).
	CIPollStall time.Duration
	// CIPollMaxBudget is the hard ceiling for the adaptive budget extension (BEH-685).
	// When it exceeds CIPollBudget, a poll that reaches the soft budget while a real
	// (non-EXPECTED) required gate is still in flight keeps polling up to this ceiling
	// instead of timing out — a slow-but-running gate (the browser-backed linting_and_tests
	// routinely outlasts the 12 min soft budget) must not be abandoned mid-run and routed to
	// manual triage. Zero (or ≤ CIPollBudget) disables the extension; CIPollBudget is then
	// the only bound (the pre-BEH-685 behaviour).
	CIPollMaxBudget time.Duration
	// DedupModel is the cheap model used for the semantic dedup pass — a small model
	// is plenty for a one-token same-class-or-NONE classification.
	DedupModel string

	// LoopPollInterval is how long the cmd/loop daemon idles before re-polling an
	// empty queue (DESIGN.md §cmd/loop config knobs).
	LoopPollInterval time.Duration
	// LoopCapBackoff is how long the daemon sleeps after a spending-cap abort before
	// re-polling — long enough for the external cap window to reset and auto-resume.
	LoopCapBackoff time.Duration
	// LoopMaxConsecutiveFailures is the circuit-breaker threshold: consecutive no-PR
	// tickets before the daemon trips and winds down.
	LoopMaxConsecutiveFailures int
	// LoopMaxTickets is the optional ceiling on *attempted* tickets; 0 = unlimited
	// (the default, since the loop is deliberately long-running). A non-zero value
	// stops the loop cleanly once reached.
	LoopMaxTickets int
	// LoopMaxRuntime is the optional wall-clock ceiling; 0 = unlimited (the default).
	// A non-zero value stops the loop cleanly once reached.
	LoopMaxRuntime time.Duration
	// LoopClaimTTL is the grace period after which an In Progress claim that produced
	// no branch/PR is reaped back to Todo — long enough to comfortably clear the
	// claim→first-push window (the observed mid-flight case was ~18 min), so a healthy
	// agent mid-work is never mistaken for a dead one. Defaults to 30m.
	LoopClaimTTL time.Duration
	// LoopDiskReclaimThreshold is the soft free-disk floor (bytes) below which the
	// daemon proactively reclaims host disk between tickets — pruning merged worktrees
	// before the hard 5 GiB sandbox preflight floor would refuse a launch (ADR-0005).
	// It defaults above that floor with headroom; 0 disables reclaim entirely.
	LoopDiskReclaimThreshold uint64
}

// Project is the Consumer's declaration, resolved: the committed
// `.agent-harness/config.toml` (ProjectConfig) after its defaults and the two
// operator overrides (HARNESS_IMAGE, PNPM_STORE_VOLUME), plus the prompt bodies
// committed beside it (ADR-0008/0009). Nothing here is a credential.
type Project struct {
	// Image is the sandbox image tag/ref the harness runs sessions in.
	Image string
	// Dockerfile is the checkout-relative path to the Consumer Dockerfile (FROM the
	// base) to build the sandbox image from on a local miss (ADR-0008). Empty means
	// the Image is a prebuilt ref the harness pulls instead of building.
	Dockerfile string
	// CacheVolume is the Docker volume name for the optional persistent toolchain
	// cache (pnpm store / GOMODCACHE / NuGet). Empty means no cache mount.
	CacheVolume string
	// CacheMountPath is the in-container path CacheVolume mounts at (Consumer-
	// declared; BEH-635). Only meaningful when CacheVolume is set.
	CacheMountPath string
	// CachePruneCommand is the Consumer's host-side cache-reclaim command, run by
	// the loop between tickets (ADR-0005). Empty skips that reclaim rung.
	CachePruneCommand string
	// BranchPrefix is the canonical worktree branch prefix (from the project
	// config's branch_prefix, default "feat"). The harness keys verify/push/PR/
	// dispatch-guards off `<BranchPrefix>/<slug>` (ADR-0008).
	BranchPrefix string
	// PostCreate is the Consumer's per-worktree toolchain-setup command the harness
	// runs in the worktree after host-side branch creation (BEH-636). It runs on
	// every provisioning pass, not only on a freshly-created worktree, so it MUST be
	// idempotent — see ProjectConfig.PostCreate for why (BEH-796). Empty means no
	// setup step.
	PostCreate string
	// Gates is the ordered, named host-side gate list from the project config.
	// BEH-631 only sources it; the runner that iterates it is BEH-634.
	Gates []Gate
	// DocsOnlyExcludedRoots are the Consumer's directory prefixes whose contents
	// are never inert prose. Empty disables the docs-only short-circuit entirely.
	DocsOnlyExcludedRoots []string
	// HandoffStripPaths are the worktree-relative paths the implementation stage
	// deletes on handoff (Consumer-declared; empty strips nothing).
	HandoffStripPaths []string
	// SourceRoots are the checkout-relative directories the resolved-symbol
	// dispatch advisory greps (Consumer-declared; empty disables the advisory).
	SourceRoots []string
	// Tracker holds the project config's non-secret tracker selection names
	// (label ids, ready/blocked labels). The tracker credential stays env-only.
	Tracker TrackerConfig
	// Feedback is the opt-in upstream-feedback surface (ADR-0011/BEH-640): where
	// harness-audience findings go (local artifact vs the public harness repo).
	// The upstream sink reuses the host's GH_TOKEN; nothing here is a credential.
	Feedback FeedbackConfig
	// Prompts holds the Consumer's per-Stage prompt bodies, read from the
	// bind-mounted checkout's `.agent-harness/prompts/` (ADR-0009). The harness
	// composes each body inside its non-overridable contract envelope.
	Prompts PromptBodies
}

const (
	defaultTddTimeout       = 30 * time.Minute
	defaultTddLargeCap      = 60 * time.Minute
	defaultReviewTimeout    = 25 * time.Minute
	defaultRetroTimeout     = 45 * time.Minute
	defaultSessionIdle      = 20 * time.Minute
	defaultModel            = "claude-opus-5-5"
	defaultDedupModel       = "claude-haiku-4-5-20251001"
	defaultCIMaxFixAttempts = 2
	defaultCIFixBudget      = 30 * time.Minute
	defaultCIPollInterval   = 15 * time.Second
	defaultCIPollBudget     = 12 * time.Minute
	// defaultCIPollStall is the no-progress window after which a frozen-but-still-pending
	// snapshot is treated as wedged (ErrPollStalled). The poll only starts this timer once
	// the snapshot carries stall evidence — a real gate has settled or an EXPECTED context
	// is present (stallEvidence, BEH-620) — so it never trips on a slow cold start. It must
	// comfortably exceed the gap between consecutive heavy-gate completions: the real gates
	// here (full build + prerender, browser-backed storybook, e2e) each run well over 4 min,
	// so a 4 min window false-positived between flips on a healthy run. 8 min gives generous
	// headroom while staying below CIPollBudget (12 min) so a genuine wedge still bails early.
	defaultCIPollStall = 8 * time.Minute
	// defaultCIPollMaxBudget is the hard ceiling the adaptive extension (BEH-685) polls to
	// while a real required gate is still in flight past the 12 min soft budget. The
	// browser-backed linting_and_tests (full oxlint + Storybook + Playwright + boundary
	// guards) is the long pole and observably outran 12 min on a healthy PR, dumping it to
	// manual triage. 25 min comfortably exceeds that job's wall-clock while staying under
	// defaultCIFixBudget (30 min) so the extension can't outlive the whole watch+fix loop.
	defaultCIPollMaxBudget = 25 * time.Minute

	defaultLoopPollInterval           = 60 * time.Second
	defaultLoopCapBackoff             = 45 * time.Minute
	defaultLoopMaxConsecutiveFailures = 3
	defaultLoopMaxTickets             = 0 // unlimited
	defaultLoopMaxRuntime             = time.Duration(0)
	defaultLoopClaimTTL               = 30 * time.Minute
	defaultStopFile                   = ProjectDirName + "/STOP"
	// 8 GiB: the 5 GiB MinFreeDiskBytes sandbox floor plus headroom, so reclaim fires
	// before a launch would ever be refused (ADR-0005).
	defaultLoopDiskReclaimThreshold uint64 = 8 << 30
)

// Getenv looks up an environment variable by name, returning "" when unset.
type Getenv func(string) string

func requireEnv(get Getenv, key string) (string, error) {
	v := get(key)
	if v == "" {
		return "", fmt.Errorf("missing required env var: %s", key)
	}
	return v, nil
}

// Option adjusts how Load resolves configuration. It exists so a test can pin the
// harness version the Consumer's `min_harness_version` is checked against, rather
// than depending on the linker flags of the binary under test.
type Option func(*loadOptions)

type loadOptions struct {
	harnessVersion string
}

// WithHarnessVersion overrides the harness version used for the compatibility
// check. Production passes nothing and gets version.Version.
func WithHarnessVersion(v string) Option {
	return func(o *loadOptions) { o.harnessVersion = v }
}

// Load resolves the harness config: the Host half from the environment, then the
// Project half from the Consumer checkout at PROJECT_PATH. Any misconfiguration in
// either fails here, before a sandbox launches.
func Load(get Getenv, opts ...Option) (Config, error) {
	options := loadOptions{harnessVersion: version.Version}
	for _, opt := range opts {
		opt(&options)
	}
	host, err := loadHost(get)
	if err != nil {
		return Config{}, err
	}
	project, err := loadProject(get, host.ProjectPath, options.harnessVersion)
	if err != nil {
		return Config{}, err
	}
	return Config{Host: host, Project: project}, nil
}

// loadHost reads the harness side's half from the environment. The Claude
// credential is the only secret that crosses the sandbox boundary (ADR-0002); it
// is validated for presence here but not returned — it flows into the container
// via docker's `-e NAME` reading the harness's own inherited environment, so it
// never sits in our argv. GH_TOKEN is validated host-side too but stays host-only
// (the harness's own push + `gh pr create`); it never enters the container.
func loadHost(get Getenv) (Host, error) {
	// Exactly one Claude credential is required: a long-lived API key
	// (ANTHROPIC_API_KEY) or a subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN,
	// from `claude setup-token`). The latter must NOT be set as ANTHROPIC_API_KEY
	// — Claude Code would send it via x-api-key and Anthropic rejects it (BEH-316).
	if get("ANTHROPIC_API_KEY") == "" && get("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return Host{}, fmt.Errorf(
			"missing Claude credential: set ANTHROPIC_API_KEY (sk-ant-api03-…) " +
				"or CLAUDE_CODE_OAUTH_TOKEN (sk-ant-oat01-… from `claude setup-token`)",
		)
	}
	h := Host{
		// The tracker credential is NOT required here: which one is needed depends
		// on the Consumer's `tracker.kind`, which lives in the project config.
		// Requiring LINEAR_API_KEY unconditionally meant a Jira- or
		// GitHub-Issues-only Consumer could not start the harness at all (BEH-641).
		// trackers.New owns the per-kind check, alongside the Jira triple's.
		LinearAPIKey:    get("LINEAR_API_KEY"),
		JiraBaseURL:     get("JIRA_BASE_URL"),
		JiraEmail:       get("JIRA_EMAIL"),
		JiraAPIToken:    get("JIRA_API_TOKEN"),
		AnthropicAPIKey: get("ANTHROPIC_API_KEY"),
	}
	var err error
	if h.GitHubToken, err = requireEnv(get, "GH_TOKEN"); err != nil {
		return Host{}, err
	}
	if h.ProjectPath, err = requireEnv(get, "PROJECT_PATH"); err != nil {
		return Host{}, err
	}
	for _, k := range h.knobs(get) {
		if err := k.resolve(get(k.key)); err != nil {
			return Host{}, err
		}
	}
	if err := h.validateIdleBelowCaps(); err != nil {
		return Host{}, err
	}
	return h, nil
}

// knobs is the table of Host's env-tunable settings, one row per knob: the env
// var, the field it sets, how its value parses, the default when it is unset, and
// whether a value the parser rejects is an error or falls back to the default.
//
// The session caps and CI knobs are lenient, a fallback-on-junk contract that
// predates strict parsing. The cmd/loop knobs are strict: a nonsensical override
// fails loud at load, before the daemon launches.
func (h *Host) knobs(get Getenv) []knob {
	// HARNESS_MODEL moves every stage at once; a per-stage variable overrides it.
	sharedModel := orDefault(get("HARNESS_MODEL"), defaultModel)

	return []knob{
		bind("TDD_TIMEOUT_MS", &h.TddTimeout, positiveMillis, defaultTddTimeout, lenient),
		bind("TDD_LARGE_REFACTOR_TIMEOUT_MS", &h.TddLargeRefactorTimeout, positiveMillis, defaultTddLargeCap, lenient),
		bind("REVIEW_TIMEOUT_MS", &h.ReviewTimeout, positiveMillis, defaultReviewTimeout, lenient),
		bind("RETROSPECTIVE_TIMEOUT_MS", &h.RetrospectiveTimeout, positiveMillis, defaultRetroTimeout, lenient),
		bind("SESSION_IDLE_TIMEOUT_MS", &h.SessionIdleTimeout, positiveMillis, defaultSessionIdle, lenient),

		bind("TDD_MODEL", &h.ImplementationModel, text, sharedModel, lenient),
		bind("REVIEW_MODEL", &h.ReviewModel, text, sharedModel, lenient),
		bind("RETROSPECTIVE_MODEL", &h.RetrospectiveModel, text, sharedModel, lenient),
		bind("DEDUP_MODEL", &h.DedupModel, text, defaultDedupModel, lenient),

		bind("CI_MAX_FIX_ATTEMPTS", &h.CIMaxFixAttempts, positiveCount, defaultCIMaxFixAttempts, lenient),
		bind("CI_FIX_BUDGET_MS", &h.CIFixBudget, positiveMillis, defaultCIFixBudget, lenient),
		bind("CI_POLL_INTERVAL_MS", &h.CIPollInterval, positiveMillis, defaultCIPollInterval, lenient),
		bind("CI_POLL_BUDGET_MS", &h.CIPollBudget, positiveMillis, defaultCIPollBudget, lenient),
		bind("CI_POLL_STALL_MS", &h.CIPollStall, positiveMillis, defaultCIPollStall, lenient),
		bind("CI_POLL_MAX_BUDGET_MS", &h.CIPollMaxBudget, positiveMillis, defaultCIPollMaxBudget, lenient),

		bind("LOOP_POLL_INTERVAL_MS", &h.LoopPollInterval, positiveMillis, defaultLoopPollInterval, strict),
		bind("LOOP_CAP_BACKOFF_MS", &h.LoopCapBackoff, positiveMillis, defaultLoopCapBackoff, strict),
		bind("LOOP_MAX_CONSECUTIVE_FAILURES", &h.LoopMaxConsecutiveFailures, positiveCount, defaultLoopMaxConsecutiveFailures, strict),
		bind("LOOP_MAX_TICKETS", &h.LoopMaxTickets, ceilingCount, defaultLoopMaxTickets, strict),
		bind("LOOP_MAX_RUNTIME_MS", &h.LoopMaxRuntime, ceilingMillis, defaultLoopMaxRuntime, strict),
		bind("LOOP_CLAIM_TTL_MS", &h.LoopClaimTTL, positiveMillis, defaultLoopClaimTTL, strict),
		bind("LOOP_DISK_RECLAIM_THRESHOLD_BYTES", &h.LoopDiskReclaimThreshold, reclaimBytes, defaultLoopDiskReclaimThreshold, strict),
		bind("STOP_FILE", &h.StopFile, text, defaultStopFile, strict),
	}
}

// loadProject resolves the Consumer's half from the checkout (ADR-0008): the
// committed config, the version pin it carries, and the per-Stage prompt bodies.
// A missing or invalid file fails loud here, before any sandbox launches, rather
// than surfacing as a wrong image or empty gate list later.
func loadProject(get Getenv, checkout, harnessVersion string) (Project, error) {
	pc, err := LoadProject(checkout)
	if err != nil {
		return Project{}, err
	}

	// The Consumer's version pin is checked before anything else reads the config,
	// so an incompatible harness says so instead of acting on keys it half
	// understands.
	if err := checkHarnessVersion(harnessVersion, pc.MinHarnessVersion); err != nil {
		return Project{}, err
	}

	// Per-Stage prompt bodies live alongside the project config in the checkout
	// (ADR-0009). Each one is required: the harness declares no skill of its own,
	// so a missing body would leave that Stage with nothing to invoke.
	prompts, err := LoadPrompts(checkout)
	if err != nil {
		return Project{}, err
	}

	// The PNPM_STORE_VOLUME env override is the host twin of the deprecated
	// pnpm_store_volume key: if it supplies the cache volume but the project
	// declared no `[cache]` path, default the mount to /pnpm-store — mirroring
	// applyDefaults' back-compat mapping so an env override alone can't produce a
	// pathless `-v <vol>:` that dies at docker run (exit 125) after the claim.
	cacheVolume := orDefault(get("PNPM_STORE_VOLUME"), pc.Cache.Volume)
	cachePath := pc.Cache.Path
	if cachePath == "" && cacheVolume != "" {
		cachePath = legacyPnpmStoreMountPath
	}

	return Project{
		Image:                 orDefault(get("HARNESS_IMAGE"), pc.Image),
		Dockerfile:            pc.Dockerfile,
		CacheVolume:           cacheVolume,
		CacheMountPath:        cachePath,
		CachePruneCommand:     pc.Cache.PruneCommand,
		BranchPrefix:          pc.BranchPrefix,
		PostCreate:            pc.PostCreate,
		Gates:                 pc.Gates,
		DocsOnlyExcludedRoots: pc.DocsOnlyExcludedRoots,
		HandoffStripPaths:     pc.HandoffStripPaths,
		SourceRoots:           pc.SourceRoots,
		Tracker:               pc.Tracker,
		Feedback:              pc.Feedback,
		Prompts:               prompts,
	}, nil
}

// validateIdleBelowCaps enforces the watchdog's load-bearing invariant: the idle/
// no-progress window must stay strictly below every per-session hard cap. The idle
// watchdog only reaps a dead-stream session if its cap leaves room for the idle
// window to fire first; once the idle window reaches a cap, that cap is always hit
// first (watchReason uses >=), silently disabling dead-stream detection for that
// session class — the inert-watchdog regression that let a stalled retrospective
// burn to the hard cap instead of being reaped early (BEH-535/538). A tuning typo
// in an env override must fail loud here, before any sandbox launches, rather than
// surface an hour later as a hung session.
func (h Host) validateIdleBelowCaps() error {
	caps := []struct {
		name string
		cap  time.Duration
	}{
		{"TDD_TIMEOUT_MS", h.TddTimeout},
		{"TDD_LARGE_REFACTOR_TIMEOUT_MS", h.TddLargeRefactorTimeout},
		{"REVIEW_TIMEOUT_MS", h.ReviewTimeout},
		{"RETROSPECTIVE_TIMEOUT_MS", h.RetrospectiveTimeout},
	}
	for _, c := range caps {
		if h.SessionIdleTimeout >= c.cap {
			return fmt.Errorf(
				"SESSION_IDLE_TIMEOUT (%s) must be below the %s cap (%s), else the idle/no-progress watchdog can never fire before that hard cap and a stalled session is reaped only at the cap",
				h.SessionIdleTimeout, c.name, c.cap,
			)
		}
	}
	return nil
}

// checkHarnessVersion enforces a Consumer's `min_harness_version` pin against the
// running harness. An empty pin is no constraint, and a from-source build (no
// ldflags, so version.DevVersion) cannot be compared — the maintainer working on
// the harness itself must not be blocked by a Consumer's floor.
func checkHarnessVersion(harnessVersion, pin string) error {
	if pin == "" {
		return nil
	}
	if version.IsDev(harnessVersion) {
		fmt.Fprintf(os.Stderr,
			"note: this project requires agent-harness >= %s (min_harness_version); "+
				"skipping the check because this is an unversioned build\n", pin)
		return nil
	}
	ok, err := version.AtLeast(harnessVersion, pin)
	if err != nil {
		return fmt.Errorf("reading min_harness_version from the project config: %w", err)
	}
	if !ok {
		return fmt.Errorf(
			"this project requires agent-harness >= %s (min_harness_version in %s/config.toml), "+
				"but this binary is %s — a harness older than the config it reads ignores the keys it "+
				"does not know. Download a newer release: https://github.com/danoleary/agent-harness/releases",
			pin, ProjectDirName, harnessVersion,
		)
	}
	return nil
}

// EnvFileName is the basename of the operator's credential file, and
// EnvConfigDirName the directory it sits in under the operator's config dir.
const (
	EnvFileName      = ".env"
	EnvConfigDirName = "agent-harness"
)

// ResolveEnvFile returns the env file to load, or "" when there is none.
//
// An operator who installed from a release archive has no checkout to keep a
// `.env` beside, and the one place it must never live is the Consumer's own
// repository: every stage bind-mounts that checkout into its sandbox (ADR-0002),
// so a credential file there would hand the host-only tracker and GitHub tokens
// to the agent session as a readable file. The lookup therefore keys off the
// operator, not the project:
//
//  1. $HARNESS_ENV_FILE — explicit, so one operator can hold a credential set per
//     project, each outside every checkout.
//  2. <workingDir>/.env — how every from-source operator already runs. Local beats
//     global, the usual precedence, so adding a config-dir file later cannot
//     silently repoint an existing setup at another project.
//  3. <workingDir>/.agent-harness/.env, then $PROJECT_PATH/.agent-harness/.env —
//     the in-repo location, beside the config and prompts the Consumer already
//     commits. The sandbox cannot read it: every container masks that exact path
//     (sandbox.maskArgs), which is what makes keeping it in the repo safe. The
//     cwd form covers running the harness from the project; the PROJECT_PATH form
//     covers running it from anywhere with PROJECT_PATH already exported.
//  4. $XDG_CONFIG_HOME/agent-harness/.env, else ~/.config/agent-harness/.env —
//     the operator-scoped home, for someone driving several projects from one
//     install. XDG_CONFIG_HOME is usually unset on macOS, so the ~/.config form
//     is what most operators get.
//
// scripts/loop-start.sh resolves PROJECT_PATH in this same order. The two must
// agree: if the script read one file and the daemon another, the pidfile and log
// would land under a different project than the one being worked, and the viewer
// would report a running daemon as stopped.
//
// Only an existing file is returned, so an operator who exports the variables
// directly gets "" and no file is read.
func ResolveEnvFile(getenv Getenv, workingDir string) string {
	if explicit := getenv("HARNESS_ENV_FILE"); explicit != "" {
		if fileExists(explicit) {
			return explicit
		}
	}

	if workingDir != "" {
		candidate := filepath.Join(workingDir, EnvFileName)
		if fileExists(candidate) {
			return candidate
		}
	}

	// The in-repo location, from cwd and then from an exported PROJECT_PATH.
	for _, root := range []string{workingDir, getenv("PROJECT_PATH")} {
		if root == "" {
			continue
		}
		candidate := InRepoEnvFile(root)
		if fileExists(candidate) {
			return candidate
		}
	}

	if candidate := EnvFileHint(getenv); candidate != "" {
		if fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

// InRepoEnvFile is the credential file a Consumer may commit-adjacent inside its
// own checkout: `<checkout>/.agent-harness/.env`. It is the one path inside the
// mount that every container masks, so it is the only in-repo location the harness
// vouches for. It must stay in step with sandbox.ProjectDirName /
// sandbox.CredentialFileName, which build the mask — the two are the same contract
// seen from the host and from the container.
func InRepoEnvFile(checkoutPath string) string {
	return filepath.Join(checkoutPath, ProjectDirName, EnvFileName)
}

// EnvFileHint is the path the operator SHOULD keep credentials in: the config-dir
// location ResolveEnvFile prefers. The bind-mount warning quotes it, so the two
// must agree or the advice does not fix the problem.
func EnvFileHint(getenv Getenv) string {
	configHome := getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home := getenv("HOME")
		if home == "" {
			// No HOME and no XDG_CONFIG_HOME: there is no config dir to name, and
			// returning "/.config/…" would send an operator to a path they cannot
			// write. The caller treats "" as "no candidate".
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, EnvConfigDirName, EnvFileName)
}

// EnvFileInsideProject reports whether envFile sits inside projectPath, which is
// the bind-mount hazard: the whole Consumer checkout is mounted into every sandbox
// at its real path (ADR-0002), so a credential file under it is readable by the
// agent session.
//
// The comparison is path-segment-wise, not a string prefix: `/src/herd-notes` is
// not inside `/src/herd`, and treating it as inside would warn an operator who did
// nothing wrong.
func EnvFileInsideProject(envFile, projectPath string) bool {
	if envFile == "" || projectPath == "" {
		return false
	}
	// The blessed in-repo path is masked in every container, so it is not an
	// exposure. Warning about the documented location would train an operator to
	// ignore the warning that matters.
	if filepath.Clean(envFile) == InRepoEnvFile(filepath.Clean(projectPath)) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(projectPath), filepath.Clean(envFile))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// fileExists reports whether path is an existing regular file. A directory named
// `.env` is not a credential file, so it does not count.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// LoadDotEnv loads KEY=VALUE pairs from a .env file into the process environment
// for any key not already set (mirrors `node --env-file-if-exists`). An absent
// file is a no-op. It is shared by the cmd entrypoints so each stays a thin
// wrapper. Call it before Load so the file fills any gaps the real env leaves.
func LoadDotEnv(path string) {
	if path == "" {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
