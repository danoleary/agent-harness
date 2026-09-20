package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// ProjectConfig is the committed, per-Consumer configuration read from the
// bind-mounted checkout at `.agent-harness/config.toml` (ADR-0008). It carries
// the project-specific values the harness used to hardcode as Go constants:
// the sandbox image, the pnpm store volume, the branch prefix, the named gate
// list, and the tracker's non-secret selection names. Secrets and host paths
// stay env-only (see Load) — nothing in here is a credential.
type ProjectConfig struct {
	// Image is the sandbox image tag/ref. A Consumer either names a prebuilt,
	// compatible image here (the harness PULLS it on a local miss) or commits a
	// Dockerfile below (the harness BUILDS it) — see ADR-0008. When a dockerfile
	// is also set, this is the local tag the harness builds to and runs.
	Image string `toml:"image"`
	// Dockerfile is the checkout-relative path to a Consumer Dockerfile that
	// `FROM`s the published base image (ADR-0007/0008). When set, the harness
	// builds the sandbox image on a local miss instead of pulling it. Declaring
	// neither Image nor Dockerfile is a hard error (validate).
	Dockerfile string `toml:"dockerfile"`
	// Cache is the optional persistent cache volume mounted into every sandbox +
	// gate container (BEH-635). It generalizes the herd-specific pnpm store into a
	// Consumer-declared volume name + mount path, so a NuGet/GOMODCACHE/pnpm
	// Consumer differs only in config.
	Cache CacheConfig `toml:"cache"`
	// PnpmStoreVolume is the deprecated herd-specific alias for the cache volume
	// name; when set (and `[cache]` is absent) it maps onto Cache with the
	// historical /pnpm-store mount, so herd's committed config keeps working
	// unchanged (BEH-635). New Consumers should use `[cache]`.
	PnpmStoreVolume string `toml:"pnpm_store_volume"`
	// BranchPrefix is the canonical worktree branch prefix (was the hardcoded
	// "feat"); the harness keys verify/push/PR/dispatch-guards off
	// `<branch_prefix>/<slug>`.
	BranchPrefix string `toml:"branch_prefix"`
	// PostCreate is the Consumer's per-worktree toolchain-setup command, run by the
	// harness in the worktree after it creates the branch host-side
	// (ADR-0008/BEH-636). It replaces the old coupling where the sandbox agent ran
	// herd's `scripts/new-worktree.sh`: herd's env links + `pnpm install` +
	// Playwright install now live here as a declared, language-agnostic command
	// (`$PROJECT_PATH` is available in the run). Empty means no setup step.
	//
	// It MUST be idempotent. The harness runs it on every provisioning pass, not only
	// on a freshly-created worktree, because a resumed worktree routinely has its deps
	// missing — the successful-handoff strip removes them (BEH-412) and an OOM-killed
	// install never wrote them (BEH-523) — while still looking ready from the outside
	// (BEH-796). Prefer lockfile-frozen, already-satisfied-is-a-no-op commands.
	PostCreate string `toml:"post_create"`
	// Gates is the ordered, named host-side gate list (was the hardcoded
	// `pnpm check && pnpm typecheck`). BEH-631 only sources the list from
	// config; iterating it in the runner is BEH-634.
	Gates []Gate `toml:"gates"`
	// DocsOnlyExcludedRoots lists the directory prefixes whose contents can feed a
	// gate or a CI job whatever they look like — the Consumer's source trees, its
	// scripts, its workflows. The harness skips the review gate re-run and the CI
	// poll for a branch whose whole diff is inert prose OUTSIDE these roots.
	//
	// It was a hardcoded herd list (`web/`, `supabase/`, …), which is a fact about
	// one Consumer's layout, not about the harness. Leaving it EMPTY disables the
	// short-circuit — the safe direction, since the cost of a wrong skip is
	// merging past a job that could go red (see git.DocsOnlyPaths). Keep it a
	// superset of every root your workflows' `on.pull_request.paths` trigger on.
	DocsOnlyExcludedRoots []string `toml:"docs_only_excluded_roots"`
	// HandoffStripPaths lists worktree-relative paths the implementation stage
	// deletes on handoff, so a reviewer on a different platform never inherits the
	// sandbox's linux-arm64 build artifacts (BEH-412). Which paths those are is a
	// Consumer fact — `web/node_modules` for a pnpm monorepo, `obj`/`bin` for .NET,
	// nothing at all for Go — so an empty list strips nothing (BEH-641). The
	// Consumer's post_create re-provisions them before the review session.
	HandoffStripPaths []string `toml:"handoff_strip_paths"`
	// SourceRoots lists the checkout-relative directories the resolved-symbol
	// dispatch advisory greps when a ticket cites code symbols (BEH-544). Omitting
	// it DISABLES the advisory rather than guessing a path: a missed nudge costs
	// one session, while scanning the wrong tree reports every cited symbol absent
	// and pushes a valid ticket toward "recommend close" (BEH-641).
	SourceRoots []string `toml:"source_roots"`

	// MinHarnessVersion is the oldest harness this config is written for, e.g.
	// "0.2". Optional, and most Consumers will not set it until a compatibility
	// break gives them a reason to.
	//
	// It exists because an older harness reading a newer config does not fail: TOML
	// keys it does not know are ignored, so a renamed field reverts to a default and
	// an unknown gate is skipped, while the run still reports success. The pin turns
	// that into an error the operator reads at startup instead of a wrong run they
	// find out about later.
	MinHarnessVersion string `toml:"min_harness_version"`
	// Tracker holds the non-secret tracker selection names (was the hardcoded
	// Linear label UUIDs + ready/blocked label names).
	Tracker TrackerConfig `toml:"tracker"`
	// Feedback holds the opt-in upstream-feedback surface (ADR-0011/BEH-640): where
	// harness-audience findings go. Default (empty/`off`) keeps them in the local
	// artifact dir; `github` files them to the configured public harness repo.
	Feedback FeedbackConfig `toml:"feedback"`
}

// FeedbackConfig selects where the harness routes harness-audience findings
// (ADR-0011). Nothing here is a credential: upstreaming reuses the host's existing
// GH_TOKEN (a public repo needs only `public_repo` scope), which also attributes
// the issue to the reporting project as provenance.
type FeedbackConfig struct {
	// Upstream is the harness-findings sink: "off" (default) keeps them local in
	// `.agent-harness/harness-findings/`; "github" files them as issues on Repo.
	Upstream string `toml:"upstream"`
	// Repo is the public harness repo ("owner/name") harness findings are filed to
	// when Upstream is "github". Required for that mode, ignored otherwise.
	Repo string `toml:"repo"`
	// FindingsLabel is the label applied to upstreamed issues (and the scope
	// SearchFindings dedups within), the upstream twin of tracker.findings_label_id.
	// Optional — an empty label files/dedups without one.
	FindingsLabel string `toml:"findings_label"`
	// Project is the reporting-project name stamped on upstreamed filings and
	// recurrence comments, so a cross-project recurrence reads "Recurred in <project>"
	// (ADR-0011's project-tagged dedup). Optional — empty falls back to the worked
	// ticket identifier.
	Project string `toml:"project"`
}

// upstreamOff / upstreamGitHub are the recognized feedback.upstream modes.
const (
	upstreamOff    = "off"
	upstreamGitHub = "github"
)

// CacheConfig is the optional persistent cache volume the harness mounts into
// every sandbox + gate container (BEH-635). Volume is the Docker volume name;
// Path is the in-container mount point the toolchain's cache env points at
// (pnpm store, GOMODCACHE, NuGet packages, …). Both empty => no cache mount.
type CacheConfig struct {
	Volume string `toml:"volume"`
	Path   string `toml:"path"`
	// PruneCommand is the Consumer's cache-reclaim command, run between tickets by
	// the loop daemon as the cheap non-destructive rung of the disk-reclaim ladder
	// (ADR-0005) — `pnpm store prune`, `dotnet nuget locals all --clear`, `go clean
	// -modcache`. It was a hardcoded `pnpm store prune` until BEH-641. Empty skips
	// that rung entirely, leaving the worktree prune and the Docker prune.
	//
	// NOTE: unlike gates and post_create, this runs on the HOST, not in a sandbox
	// — the caches it reclaims are the host's. It is shell-interpreted, so treat it
	// with the same care as any command you would put in a Makefile.
	PruneCommand string `toml:"prune_command"`
}

// Gate is one named host-side gate command.
type Gate struct {
	Name    string `toml:"name"`
	Command string `toml:"command"`
}

// TrackerConfig is the non-secret tracker selection surface. The API key stays
// env-only (LINEAR_API_KEY / GH_TOKEN); only the selection names live here.
type TrackerConfig struct {
	// Kind selects the tracker adapter (linear, github, or jira).
	Kind string `toml:"kind"`
	// FindingsLabelID is the tracker label every harness finding is filed under.
	// The Linear adapter reads it as a label UUID; the GitHub and Jira adapters read
	// it as a label name (was linear.agentHarnessLabelID).
	FindingsLabelID string `toml:"findings_label_id"`
	// FindingsLabel is the same findings label by NAME, and is linear-only: the
	// Linear create API takes a label UUID while its issue filter matches labels by
	// name, so the adapter needs both spellings. GitHub and Jira read
	// findings_label_id as a name already and ignore this. Empty disables the
	// Linear dedup search rather than widening it to the whole team.
	FindingsLabel string `toml:"findings_label"`
	// TeamKey is the Linear team the selector and the stale-claim reaper are scoped
	// to (e.g. "BEH"), and is linear-only — GitHub scopes by repo and Jira by
	// project key. Required for kind=linear (was selection.harnessTeamKey).
	TeamKey string `toml:"team_key"`
	// ReadyLabel is the human-applied blast-radius gate label the selector
	// requires (was selection.agentReadyLabel).
	ReadyLabel string `toml:"ready_label"`
	// BlockedLabel marks a ticket a human flagged as blocked; never auto-worked
	// (was selection.blockedLabel).
	BlockedLabel string `toml:"blocked_label"`
	// Repo is the GitHub "owner/name" the GitHub adapter is bound to (github kind
	// only; ignored by Linear, which scopes by team). Findings are filed back into
	// it and the ready queue is read from it.
	Repo string `toml:"repo"`
	// InProgressLabel models the claim for trackers with no workflow states
	// (GitHub): the label added on claim / removed on release, since GitHub issues
	// have only open/closed. Ignored by Linear, which uses real workflow states.
	InProgressLabel string `toml:"in_progress_label"`
	// Assignee is the optional bot login the GitHub adapter assigns on claim and
	// clears on release (the "+ assignee" half of the claim semantics). Empty means
	// the label transition alone claims the ticket.
	Assignee string `toml:"assignee"`

	// The fields below are jira-only (ignored by Linear/GitHub). The Jira auth
	// secrets (base URL + email + token) are NOT here — they are host-only env
	// secrets (JIRA_BASE_URL / JIRA_EMAIL / JIRA_API_TOKEN), never committed config.

	// ProjectKey is the Jira project findings are filed into and the ticket's
	// container (the Jira twin of a GitHub repo / Linear team).
	ProjectKey string `toml:"project_key"`
	// ReadyJQL is the "ready for the agent" queue expressed as a JQL query — the
	// Jira twin of the GitHub ready label. It encodes readiness/blocked/ordering.
	ReadyJQL string `toml:"ready_jql"`
	// InProgressJQL is the JQL for the agent-claimed In Progress set the reaper reads.
	InProgressJQL string `toml:"in_progress_jql"`
	// FindingsIssueType is the Jira issue type new findings are created as (Jira
	// requires one). Empty defaults to "Task".
	FindingsIssueType string `toml:"findings_issue_type"`
	// InProgressTransition / TodoTransition / CanceledTransition are the workflow
	// transition NAMES that claim / release / cancel a ticket (Jira has no label
	// column — the claim is a status transition). Named, not id: transition ids are
	// instance-specific and vary by source status.
	InProgressTransition string `toml:"in_progress_transition"`
	TodoTransition       string `toml:"todo_transition"`
	CanceledTransition   string `toml:"cancel_transition"`
}

// ProjectDirName is the single directory the harness owns inside a Consumer
// checkout (ADR-0008): the committed surface (config.toml, prompts/) and the
// runtime artifacts it writes per project (logs/, the STOP sentinel,
// harness-findings/). It is one name so a Consumer has exactly one path to
// gitignore the runtime half of, and so nothing the harness writes lands
// outside a directory the Consumer opted into by creating.
//
// The runtime artifacts used to live under `<checkout>/agent-harness/`, the
// harness's own in-tree source dir. That only worked while the harness WAS a
// subdirectory of its one Consumer; once extracted (ADR-0007) it would have
// recreated a phantom source-looking directory inside every Consumer repo.
const ProjectDirName = ".agent-harness"

// ProjectDir is the harness directory inside a Consumer checkout.
func ProjectDir(checkoutPath string) string {
	return filepath.Join(checkoutPath, ProjectDirName)
}

// LoadProject reads and parses `<checkoutPath>/.agent-harness/config.toml`. It
// fails loud on a missing or malformed file — a Consumer with no committed
// config is a misconfiguration, not a defaultable state (ADR-0008).
func LoadProject(checkoutPath string) (ProjectConfig, error) {
	path := filepath.Join(ProjectDir(checkoutPath), "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return ProjectConfig{}, fmt.Errorf("reading project config %s: %w", path, err)
	}
	var pc ProjectConfig
	if err := toml.Unmarshal(raw, &pc); err != nil {
		return ProjectConfig{}, fmt.Errorf("parsing project config %s: %w", path, err)
	}
	pc.applyDefaults()
	if err := pc.validate(); err != nil {
		return ProjectConfig{}, fmt.Errorf("invalid project config %s: %w", path, err)
	}
	return pc, nil
}

// defaultBranchPrefix is the canonical worktree branch prefix when a Consumer
// omits branch_prefix (ADR-0008). It matches the harness's historical hardcoded
// "feat" so herd's behaviour is unchanged.
const defaultBranchPrefix = "feat"

// legacyPnpmStoreMountPath is the container mount the herd pnpm store historically
// used. It is the default Cache.Path ONLY for the deprecated `pnpm_store_volume`
// alias, so herd's committed config keeps mounting at /pnpm-store unchanged; the
// generalized `[cache]` surface has no implicit path (validate requires it).
const legacyPnpmStoreMountPath = "/pnpm-store"

func (pc *ProjectConfig) applyDefaults() {
	if pc.BranchPrefix == "" {
		pc.BranchPrefix = defaultBranchPrefix
	}
	// Default the feedback sink to local-only: nothing leaves the repo without an
	// explicit `feedback.upstream = "github"` opt-in (ADR-0011).
	if pc.Feedback.Upstream == "" {
		pc.Feedback.Upstream = upstreamOff
	}
	// Map the deprecated herd-specific pnpm_store_volume onto the generalized
	// cache surface, defaulting to its historical /pnpm-store mount.
	if pc.Cache.Volume == "" && pc.PnpmStoreVolume != "" {
		pc.Cache.Volume = pc.PnpmStoreVolume
		if pc.Cache.Path == "" {
			pc.Cache.Path = legacyPnpmStoreMountPath
		}
	}
}

// validate fails loud on a Consumer misconfiguration: a missing image, tracker
// kind, or gate list is not a defaultable state — there is no sane cross-project
// default (ADR-0008), so the harness must refuse to launch rather than silently
// fall back.
func (pc *ProjectConfig) validate() error {
	if pc.Image == "" && pc.Dockerfile == "" {
		return fmt.Errorf("a sandbox image source is required: set either `image` (a prebuilt ref to pull) or `dockerfile` (a Consumer Dockerfile FROM the base to build)")
	}
	if pc.Tracker.Kind == "" {
		return fmt.Errorf("tracker.kind is required")
	}
	// Linear scopes selection by team key and gates it on the ready label. Neither
	// has a defensible default — an empty team key would query every team the
	// credential can see, and an empty ready label would drop the human-applied
	// blast-radius gate — so both fail loud here rather than at the first poll.
	if pc.Tracker.Kind == "linear" {
		if pc.Tracker.TeamKey == "" {
			return fmt.Errorf("tracker.team_key is required for kind=linear (the Linear team the selector is scoped to, e.g. \"BEH\")")
		}
		if pc.Tracker.ReadyLabel == "" {
			return fmt.Errorf("tracker.ready_label is required for kind=linear (the human-applied gate that makes a ticket eligible for unattended work)")
		}
	}
	if len(pc.Gates) == 0 {
		return fmt.Errorf("at least one [[gates]] entry is required")
	}
	for i, g := range pc.Gates {
		if g.Name == "" {
			return fmt.Errorf("gates[%d].name is required", i)
		}
		if g.Command == "" {
			return fmt.Errorf("gates[%d] (%q) has no command", i, g.Name)
		}
	}
	// The generalized cache surface has no implicit mount path — a Consumer that
	// declares a volume must declare where it mounts (BEH-635).
	if pc.Cache.Volume != "" && pc.Cache.Path == "" {
		return fmt.Errorf("cache.path is required when cache.volume is set")
	}
	if err := pc.Feedback.validate(); err != nil {
		return err
	}
	return nil
}

// validate rejects the two feedback misconfigurations that would otherwise
// surface only when a finding is filed (ADR-0011): an unrecognized upstream mode,
// and github mode with no addressable public repo. off mode needs no repo — it
// never leaves the checkout.
func (fc *FeedbackConfig) validate() error {
	switch fc.Upstream {
	case upstreamOff:
		return nil
	case upstreamGitHub:
		if _, _, err := SplitOwnerRepo(fc.Repo); err != nil {
			return fmt.Errorf("feedback.repo must be \"owner/name\" when feedback.upstream = %q, got %q", upstreamGitHub, fc.Repo)
		}
		return nil
	default:
		return fmt.Errorf("feedback.upstream must be %q or %q, got %q", upstreamOff, upstreamGitHub, fc.Upstream)
	}
}

// SplitOwnerRepo parses a github "owner/name" into its parts, failing loud on a
// value that isn't exactly one owner and one name. It lives in config (a leaf
// package) so validation and the stage wiring that builds the upstream client
// share one parser rather than diverging.
func SplitOwnerRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repo must be \"owner/name\", got %q", repo)
	}
	return owner, name, nil
}
