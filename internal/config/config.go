// Package config resolves harness configuration for a single tool invocation
// (implementation, review, retrospective).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved harness configuration.
type Config struct {
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
	// Image is the sandbox image tag/ref the harness runs sessions in.
	Image string
	// Dockerfile is the checkout-relative path to the Consumer Dockerfile (FROM the
	// base) to build the sandbox image from on a local miss (ADR-0008). Empty means
	// the Image is a prebuilt ref the harness pulls instead of building.
	Dockerfile string
	// ProjectPath is the absolute host path to the herd checkout to bind-mount.
	ProjectPath string
	// CacheVolume is the Docker volume name for the optional persistent toolchain
	// cache (pnpm store / GOMODCACHE / NuGet). Empty means no cache mount.
	CacheVolume string
	// CacheMountPath is the in-container path CacheVolume mounts at (Consumer-
	// declared; BEH-635). Only meaningful when CacheVolume is set.
	CacheMountPath string
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
	// Model is the claude `--model` the tdd session runs on. Pinned to an exact
	// Opus snapshot, not the floating `opus` alias: the CLI's own default is not
	// guaranteed to be Opus and a past run silently fell back to Sonnet (BEH-316),
	// while the alias once resolved to a stale Opus 4.1 prone to a false-positive
	// usage-policy refusal on long sessions (BEH-389).
	Model string
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
	// AnthropicAPIKey is the host-only API key used for the cheap host-side semantic
	// dedup model call when filing findings (BEH-573). Unlike the sandbox credential
	// (which is validated for presence but never stored — it crosses into the
	// container via docker `-e`, ADR-0002), this is held host-side for the harness's
	// own model call. It is "" when only a subscription OAuth token is set — the
	// x-api-key header rejects an OAuth token (BEH-316) — and semantic dedup is then
	// skipped (best-effort), with filing degrading to exact key/title dedup.
	AnthropicAPIKey string
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
	// StopFile is the STOP sentinel path used by startup-clear and the stop check. A
	// relative path is resolved against ProjectPath by cmd/loop.
	StopFile string
	// LoopDiskReclaimThreshold is the soft free-disk floor (bytes) below which the
	// daemon proactively reclaims host disk between tickets — pruning merged worktrees
	// before the hard 5 GiB sandbox preflight floor would refuse a launch (ADR-0005).
	// It defaults above that floor with headroom; 0 disables reclaim entirely.
	LoopDiskReclaimThreshold uint64

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

// projectLoader resolves the Consumer's committed project config from the
// bind-mounted checkout. It is a package var so tests can inject a fixture
// instead of laying down a real .agent-harness/config.toml (ADR-0008).
var projectLoader = LoadProject

// promptsLoader resolves the Consumer's committed per-Stage prompt bodies from
// the bind-mounted checkout. A package var so tests can inject fixture bodies
// instead of laying down real .agent-harness/prompts/*.md files (ADR-0009).
var promptsLoader = LoadPrompts

const (
	defaultTddTimeout       = 30 * time.Minute
	defaultTddLargeCap      = 60 * time.Minute
	defaultReviewTimeout    = 25 * time.Minute
	defaultRetroTimeout     = 45 * time.Minute
	defaultSessionIdle      = 20 * time.Minute
	defaultModel            = "claude-opus-4-8"
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

// Load reads harness config from the environment. The Claude credential is the
// only secret that crosses the sandbox boundary (ADR-0002); it is validated for
// presence here but not returned — it flows into the container via docker's
// `-e NAME` reading the harness's own inherited environment, so it never sits in
// our argv. GH_TOKEN is validated host-side too but stays host-only (the
// harness's own push + `gh pr create`); it never enters the container.
func Load(get Getenv) (Config, error) {
	// Exactly one Claude credential is required: a long-lived API key
	// (ANTHROPIC_API_KEY) or a subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN,
	// from `claude setup-token`). The latter must NOT be set as ANTHROPIC_API_KEY
	// — Claude Code would send it via x-api-key and Anthropic rejects it (BEH-316).
	if get("ANTHROPIC_API_KEY") == "" && get("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return Config{}, fmt.Errorf(
			"missing Claude credential: set ANTHROPIC_API_KEY (sk-ant-api03-…) " +
				"or CLAUDE_CODE_OAUTH_TOKEN (sk-ant-oat01-… from `claude setup-token`)",
		)
	}
	githubToken, err := requireEnv(get, "GH_TOKEN")
	if err != nil {
		return Config{}, err
	}

	linearKey, err := requireEnv(get, "LINEAR_API_KEY")
	if err != nil {
		return Config{}, err
	}
	herdPath, err := requireEnv(get, "PROJECT_PATH")
	if err != nil {
		return Config{}, err
	}

	// Project config lives in the bind-mounted checkout, not the environment
	// (ADR-0008). A missing/invalid file fails loud here, before any sandbox
	// launches, rather than surfacing as a wrong image or empty gate list later.
	project, err := projectLoader(herdPath)
	if err != nil {
		return Config{}, err
	}

	// Per-Stage prompt bodies live alongside the project config in the checkout
	// (ADR-0009). A missing body is tolerated (the Stage runs under the envelope),
	// so this only fails loud on a real filesystem error.
	prompts, err := promptsLoader(herdPath)
	if err != nil {
		return Config{}, err
	}

	// The cmd/loop knobs are validated strictly (a nonsensical override fails loud at
	// load, before the daemon launches), unlike the lenient parseTimeout above whose
	// fallback-on-junk contract predates this slice.
	loopPoll, err := parsePositiveDurationMs(get, "LOOP_POLL_INTERVAL_MS", defaultLoopPollInterval)
	if err != nil {
		return Config{}, err
	}
	capBackoff, err := parsePositiveDurationMs(get, "LOOP_CAP_BACKOFF_MS", defaultLoopCapBackoff)
	if err != nil {
		return Config{}, err
	}
	maxFailures, err := parsePositiveIntStrict(get, "LOOP_MAX_CONSECUTIVE_FAILURES", defaultLoopMaxConsecutiveFailures)
	if err != nil {
		return Config{}, err
	}
	maxTickets, err := parseCeilingInt(get, "LOOP_MAX_TICKETS", defaultLoopMaxTickets)
	if err != nil {
		return Config{}, err
	}
	maxRuntime, err := parseCeilingDurationMs(get, "LOOP_MAX_RUNTIME_MS", defaultLoopMaxRuntime)
	if err != nil {
		return Config{}, err
	}
	diskReclaim, err := parseNonNegativeBytes(get, "LOOP_DISK_RECLAIM_THRESHOLD_BYTES", defaultLoopDiskReclaimThreshold)
	if err != nil {
		return Config{}, err
	}
	claimTTL, err := parsePositiveDurationMs(get, "LOOP_CLAIM_TTL_MS", defaultLoopClaimTTL)
	if err != nil {
		return Config{}, err
	}

	// The PNPM_STORE_VOLUME env override is the host twin of the deprecated
	// pnpm_store_volume key: if it supplies the cache volume but the project
	// declared no `[cache]` path, default the mount to /pnpm-store — mirroring
	// applyDefaults' back-compat mapping so an env override alone can't produce a
	// pathless `-v <vol>:` that dies at docker run (exit 125) after the claim.
	cacheVolume := orDefault(get("PNPM_STORE_VOLUME"), project.Cache.Volume)
	cachePath := project.Cache.Path
	if cachePath == "" && cacheVolume != "" {
		cachePath = legacyPnpmStoreMountPath
	}

	cfg := Config{
		LinearAPIKey:            linearKey,
		GitHubToken:             githubToken,
		JiraBaseURL:             get("JIRA_BASE_URL"),
		JiraEmail:               get("JIRA_EMAIL"),
		JiraAPIToken:            get("JIRA_API_TOKEN"),
		ProjectPath:             herdPath,
		Image:                   orDefault(get("HARNESS_IMAGE"), project.Image),
		Dockerfile:              project.Dockerfile,
		CacheVolume:             cacheVolume,
		CacheMountPath:          cachePath,
		TddTimeout:              parseTimeout(get("TDD_TIMEOUT_MS"), defaultTddTimeout),
		TddLargeRefactorTimeout: parseTimeout(get("TDD_LARGE_REFACTOR_TIMEOUT_MS"), defaultTddLargeCap),
		ReviewTimeout:           parseTimeout(get("REVIEW_TIMEOUT_MS"), defaultReviewTimeout),
		RetrospectiveTimeout:    parseTimeout(get("RETROSPECTIVE_TIMEOUT_MS"), defaultRetroTimeout),
		SessionIdleTimeout:      parseTimeout(get("SESSION_IDLE_TIMEOUT_MS"), defaultSessionIdle),
		Model:                   orDefault(get("TDD_MODEL"), defaultModel),

		CIMaxFixAttempts: parsePositiveInt(get("CI_MAX_FIX_ATTEMPTS"), defaultCIMaxFixAttempts),
		CIFixBudget:      parseTimeout(get("CI_FIX_BUDGET_MS"), defaultCIFixBudget),
		CIPollInterval:   parseTimeout(get("CI_POLL_INTERVAL_MS"), defaultCIPollInterval),
		CIPollBudget:     parseTimeout(get("CI_POLL_BUDGET_MS"), defaultCIPollBudget),
		CIPollStall:      parseTimeout(get("CI_POLL_STALL_MS"), defaultCIPollStall),
		CIPollMaxBudget:  parseTimeout(get("CI_POLL_MAX_BUDGET_MS"), defaultCIPollMaxBudget),

		AnthropicAPIKey: get("ANTHROPIC_API_KEY"),
		DedupModel:      orDefault(get("DEDUP_MODEL"), defaultDedupModel),

		LoopPollInterval:           loopPoll,
		LoopCapBackoff:             capBackoff,
		LoopMaxConsecutiveFailures: maxFailures,
		LoopMaxTickets:             maxTickets,
		LoopMaxRuntime:             maxRuntime,
		LoopClaimTTL:               claimTTL,
		StopFile:                   orDefault(get("STOP_FILE"), defaultStopFile),
		LoopDiskReclaimThreshold:   diskReclaim,

		BranchPrefix:          project.BranchPrefix,
		PostCreate:            project.PostCreate,
		Gates:                 project.Gates,
		DocsOnlyExcludedRoots: project.DocsOnlyExcludedRoots,
		Tracker:               project.Tracker,
		Feedback:              project.Feedback,
		Prompts:               prompts,
	}
	if err := validateIdleBelowCaps(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
func validateIdleBelowCaps(cfg Config) error {
	caps := []struct {
		name string
		cap  time.Duration
	}{
		{"TDD_TIMEOUT_MS", cfg.TddTimeout},
		{"TDD_LARGE_REFACTOR_TIMEOUT_MS", cfg.TddLargeRefactorTimeout},
		{"REVIEW_TIMEOUT_MS", cfg.ReviewTimeout},
		{"RETROSPECTIVE_TIMEOUT_MS", cfg.RetrospectiveTimeout},
	}
	for _, c := range caps {
		if cfg.SessionIdleTimeout >= c.cap {
			return fmt.Errorf(
				"SESSION_IDLE_TIMEOUT (%s) must be below the %s cap (%s), else the idle/no-progress watchdog can never fire before that hard cap and a stalled session is reaped only at the cap",
				cfg.SessionIdleTimeout, c.name, c.cap,
			)
		}
	}
	return nil
}

// LoadDotEnv loads KEY=VALUE pairs from a .env file into the process environment
// for any key not already set (mirrors `node --env-file-if-exists`). An absent
// file is a no-op. It is shared by the cmd entrypoints so each stays a thin
// wrapper. Call it before Load so the file fills any gaps the real env leaves.
func LoadDotEnv(path string) {
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

func parseTimeout(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// parsePositiveInt parses a positive integer env value, falling back on anything
// unparseable or non-positive (same lenient contract as parseTimeout).
func parsePositiveInt(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

// parsePositiveDurationMs parses a millisecond env value that MUST be a positive
// integer when set, returning a clear error otherwise (the cmd/loop knobs reject
// nonsensical overrides loudly at load, rather than silently falling back like the
// older parseTimeout). An unset key uses the fallback.
func parsePositiveDurationMs(get Getenv, key string, fallback time.Duration) (time.Duration, error) {
	raw := get(key)
	if raw == "" {
		return fallback, nil
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer (milliseconds), got %q", key, raw)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// parseCeilingDurationMs parses an optional millisecond ceiling: 0 means unlimited,
// any positive value is the ceiling, and a negative or unparseable value is an
// error. An unset key uses the fallback.
func parseCeilingDurationMs(get Getenv, key string, fallback time.Duration) (time.Duration, error) {
	raw := get(key)
	if raw == "" {
		return fallback, nil
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer (milliseconds; 0 = unlimited), got %q", key, raw)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// parseCeilingInt parses an optional integer ceiling: 0 means unlimited, any
// positive value is the ceiling, and a negative or unparseable value is an error.
// An unset key uses the fallback.
func parseCeilingInt(get Getenv, key string, fallback int) (int, error) {
	raw := get(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer (0 = unlimited), got %q", key, raw)
	}
	return n, nil
}

// parseNonNegativeBytes parses an optional byte-count env value: 0 means "disabled"
// (a documented value, not nonsensical), any positive value is the threshold, and a
// negative or unparseable value is an error naming the var. An unset key uses the
// fallback. ParseUint rejects a leading '-', so negatives fail here too.
func parseNonNegativeBytes(get Getenv, key string, fallback uint64) (uint64, error) {
	raw := get(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a non-negative integer (bytes; 0 = disable reclaim), got %q", key, raw)
	}
	return n, nil
}

// parsePositiveIntStrict parses an integer env value that MUST be positive when
// set, returning a clear error otherwise. An unset key uses the fallback.
func parsePositiveIntStrict(get Getenv, key string, fallback int) (int, error) {
	raw := get(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, raw)
	}
	return n, nil
}
