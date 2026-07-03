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
	// Image is the sandbox image tag (was defaultImage).
	Image string `toml:"image"`
	// PnpmStoreVolume is the Docker volume name for the persistent pnpm store
	// (was defaultPnpmStoreVolume).
	PnpmStoreVolume string `toml:"pnpm_store_volume"`
	// BranchPrefix is the canonical worktree branch prefix (was the hardcoded
	// "feat"); the harness keys verify/push/PR/dispatch-guards off
	// `<branch_prefix>/<slug>`.
	BranchPrefix string `toml:"branch_prefix"`
	// Gates is the ordered, named host-side gate list (was the hardcoded
	// `pnpm check && pnpm typecheck`). BEH-631 only sources the list from
	// config; iterating it in the runner is BEH-634.
	Gates []Gate `toml:"gates"`
	// Tracker holds the non-secret tracker selection names (was the hardcoded
	// Linear label UUIDs + ready/blocked label names).
	Tracker TrackerConfig `toml:"tracker"`
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

func (pc *ProjectConfig) applyDefaults() {
	if pc.BranchPrefix == "" {
		pc.BranchPrefix = defaultBranchPrefix
	}
}

// validate fails loud on a Consumer misconfiguration: a missing image, tracker
// kind, or gate list is not a defaultable state — there is no sane cross-project
// default (ADR-0008), so the harness must refuse to launch rather than silently
// fall back.
func (pc *ProjectConfig) validate() error {
	if pc.Image == "" {
		return fmt.Errorf("image is required")
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
	return nil
}
