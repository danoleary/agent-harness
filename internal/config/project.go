package config

import (
	"fmt"
	"os"
	"path/filepath"

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
	// harness in the freshly-created worktree after it creates the branch host-side
	// (ADR-0008/BEH-636). It replaces the old coupling where the sandbox agent ran
	// herd's `scripts/new-worktree.sh`: herd's env links + `pnpm install` +
	// Playwright install now live here as a declared, language-agnostic command
	// (`$HERD_PATH` is available in the run). Empty means no setup step.
	PostCreate string `toml:"post_create"`
	// Gates is the ordered, named host-side gate list (was the hardcoded
	// `pnpm check && pnpm typecheck`). BEH-631 only sources the list from
	// config; iterating it in the runner is BEH-634.
	Gates []Gate `toml:"gates"`
	// Tracker holds the non-secret tracker selection names (was the hardcoded
	// Linear label UUIDs + ready/blocked label names).
	Tracker TrackerConfig `toml:"tracker"`
}

// CacheConfig is the optional persistent cache volume the harness mounts into
// every sandbox + gate container (BEH-635). Volume is the Docker volume name;
// Path is the in-container mount point the toolchain's cache env points at
// (pnpm store, GOMODCACHE, NuGet packages, …). Both empty => no cache mount.
type CacheConfig struct {
	Volume string `toml:"volume"`
	Path   string `toml:"path"`
}

// Gate is one named host-side gate command.
type Gate struct {
	Name    string `toml:"name"`
	Command string `toml:"command"`
}

// TrackerConfig is the non-secret tracker selection surface. The API key stays
// env-only (LINEAR_API_KEY); only the selection names live here.
type TrackerConfig struct {
	// Kind selects the tracker adapter (linear today; github/jira are BEH-637/638).
	Kind string `toml:"kind"`
	// FindingsLabelID is the tracker label every harness finding is filed under
	// (was linear.agentHarnessLabelID).
	FindingsLabelID string `toml:"findings_label_id"`
	// ReadyLabel is the human-applied blast-radius gate label the selector
	// requires (was selection.agentReadyLabel).
	ReadyLabel string `toml:"ready_label"`
	// BlockedLabel marks a ticket a human flagged as blocked; never auto-worked
	// (was selection.blockedLabel).
	BlockedLabel string `toml:"blocked_label"`
}

// LoadProject reads and parses `<checkoutPath>/.agent-harness/config.toml`. It
// fails loud on a missing or malformed file — a Consumer with no committed
// config is a misconfiguration, not a defaultable state (ADR-0008).
func LoadProject(checkoutPath string) (ProjectConfig, error) {
	path := filepath.Join(checkoutPath, ".agent-harness", "config.toml")
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
	return nil
}
