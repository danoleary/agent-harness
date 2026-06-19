// Package sandbox builds the docker argv to run one sandboxed claude session.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/proc"
)

// PreflightTimeout bounds each Preflight docker probe. A wedged daemon (e.g. its
// overlay2 store gone read-only) never returns from `docker info`; without a
// deadline the probe — and the whole run — blocks forever (BEH-386: one such hang
// stranded a run for two days). 30s is generous for a healthy daemon's round-trip
// yet fails fast on a hung one, before the ticket is ever claimed.
const PreflightTimeout = 30 * time.Second

// ProbeRunner is the production runner for Preflight: each docker probe is bounded
// by PreflightTimeout so a wedged daemon fails fast rather than hanging the run
// (BEH-386). The three cmd/ tools all pass this; tests inject their own runner.
func ProbeRunner(name string, args ...string) ([]byte, error) {
	return proc.CombinedOutput(PreflightTimeout, name, args...)
}

// helpTrailerRE matches docker's generic "See 'docker run --help'." footer,
// which it prints *after* the real error line — uninformative on its own.
var helpTrailerRE = regexp.MustCompile(`^See '.*--help'\.?$`)

// ExitCannotStart is Docker's reserved exit code for "the `docker run` command
// itself failed" — daemon unreachable, image missing, or a bad flag — as
// opposed to a code returned by the process inside the container.
const ExitCannotStart = 125

// FindingsMountPath is the fixed container path the findings dropbox is mounted at.
const FindingsMountPath = "/findings"

// PnpmStoreMountPath is the fixed container path the persistent pnpm store is mounted at.
const PnpmStoreMountPath = "/pnpm-store"

// SecretEnv is the only set of secrets that ever cross the sandbox boundary
// (ADR-0002): the Claude credential and nothing else. LINEAR_API_KEY and
// GH_TOKEN are deliberately absent — Linear access never enters the container,
// and the container no longer pushes (the harness owns all remote git I/O), so
// the host's GH_TOKEN stays host-only. Both Claude credential vars are listed:
// an `sk-ant-api03-` API key (ANTHROPIC_API_KEY, x-api-key auth) and an
// `sk-ant-oat01-` subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN, Bearer
// auth — an oat token passed as ANTHROPIC_API_KEY is rejected as an invalid
// x-api-key). `docker -e NAME` forwards only host vars that are actually set,
// so whichever one the operator configured flows through and the other is a
// no-op (BEH-316).
var SecretEnv = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

// Config describes one sandboxed claude session.
type Config struct {
	// Image is the sandbox image tag.
	Image string
	// HerdPath is the host path to the herd checkout to bind-mount.
	HerdPath string
	// FindingsDir is the host path to this session's findings dropbox dir,
	// mounted at /findings.
	FindingsDir string
	// PnpmStoreVolume is the Docker volume name for the persistent pnpm store.
	PnpmStoreVolume string
	// Prompt is the `-p` prompt to hand to claude.
	Prompt string
	// Model is the claude `--model` to pin the session to (e.g. "claude-opus-4-8"). Empty
	// omits the flag and lets the CLI fall back to its account default — which is
	// not guaranteed to be Opus, so the harness always sets it (BEH-316).
	Model string
	// ContainerName is an optional `--name` so the harness can `docker kill` the
	// container on timeout.
	ContainerName string
}

// BuildDockerRunArgs builds the argv (everything after `docker`) to run one
// sandboxed claude session. Secrets are passed by name only (`-e NAME`, no
// value) so they are read from the harness's own environment at spawn time and
// never appear in the process table. The herd checkout is bind-mounted at its
// real host path (ADR-0002) so a worktree's absolute `.git` pointer resolves
// identically inside the container and on the host.
func BuildDockerRunArgs(c Config) []string {
	args := []string{"run", "--rm", "--init"}

	if c.ContainerName != "" {
		args = append(args, "--name", c.ContainerName)
	}

	for _, name := range SecretEnv {
		args = append(args, "-e", name)
	}

	// The mount path is the real host path, which the entrypoint stat-s to match
	// the runtime uid to the checkout's owner. It is not a secret, so pass it by
	// value (unlike the `-e NAME` secrets read from the harness env).
	args = append(args, "-e", "HERD_PATH="+c.HerdPath)

	args = append(args,
		"-v", c.HerdPath+":"+c.HerdPath,
		"-v", c.PnpmStoreVolume+":"+PnpmStoreMountPath,
	)

	// The findings dropbox is mounted only when the tool produces findings.
	// Review emits none (retrospective owns findings, DESIGN.md), so it passes an
	// empty FindingsDir and gets no /findings mount — there is nowhere to write,
	// which keeps its "do not write findings" steering honest.
	if c.FindingsDir != "" {
		args = append(args, "-v", c.FindingsDir+":"+FindingsMountPath)
	}

	args = append(args,
		"-w", c.HerdPath,
		c.Image,
		"claude",
		"-p", c.Prompt,
		"--dangerously-skip-permissions",
		"--output-format", "stream-json",
		"--verbose",
	)

	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}

	return args
}

// GateConfig describes the throwaway container that re-runs the quality gates on
// a reviewed branch, host-side, after the review session exits.
type GateConfig struct {
	// Image is the sandbox image tag (reused — its entrypoint matches the runtime
	// uid to the checkout owner and trusts the repo, which the gate run also needs).
	Image string
	// HerdPath is the host path to the herd checkout to bind-mount at its real path.
	HerdPath string
	// WorktreePath is the host path of the feature worktree the gates run against
	// (the branch under review). It resolves into the mounted checkout.
	WorktreePath string
	// PnpmStoreVolume is the Docker volume name for the persistent pnpm store, so
	// the gate run's `pnpm install` is a near-instant hardlink op, not a fetch.
	PnpmStoreVolume string
	// ContainerName is an optional `--name` so the harness can `docker kill` it on timeout.
	ContainerName string
}

// gateCommand is the gate the harness re-runs as ground truth: a fresh install
// (against the warm pnpm store) then the repo's own `check` + `build`. Run from
// `web/`, the source of truth for the pnpm scripts (see herd CLAUDE.md).
const gateCommand = "cd web && pnpm install --frozen-lockfile && pnpm run check && pnpm run build"

// BuildGateRunArgs builds the argv (everything after `docker`) for the throwaway
// container that re-runs `pnpm check && pnpm build` on the reviewed branch. This
// is the harness's OWN ground truth — never the agent's self-report — and the
// push gate (DESIGN.md). The container carries NO secrets at all (not even the
// Claude credential): it runs no model, only the gates, so nothing needs to cross
// the boundary. It runs in the worktree (the branch under review), not the main
// checkout.
func BuildGateRunArgs(c GateConfig) []string {
	args := []string{"run", "--rm", "--init"}

	if c.ContainerName != "" {
		args = append(args, "--name", c.ContainerName)
	}

	// HERD_PATH lets the shared entrypoint stat the mount to match the runtime uid
	// to the checkout owner. It is the mount path, not a secret — passed by value.
	args = append(args, "-e", "HERD_PATH="+c.HerdPath)

	args = append(args,
		"-v", c.HerdPath+":"+c.HerdPath,
		"-v", c.PnpmStoreVolume+":"+PnpmStoreMountPath,
		"-w", c.WorktreePath,
		c.Image,
		"bash", "-lc", gateCommand,
	)

	return args
}

// BuildImage builds the sandbox image, streaming docker's progress so the
// operator sees the (multi-minute) build rather than a silent hang. It is the
// production builder; Preflight tests inject a fake. buildContext is the
// directory holding the harness Dockerfile (the agent-harness dir).
func BuildImage(image, buildContext string) error {
	fmt.Fprintf(os.Stderr,
		"sandbox image %q not present — building it now from %s (first run, or it was pruned; this takes a few minutes)…\n",
		image, buildContext,
	)
	cmd := exec.Command("docker", "build", "-t", image, buildContext)
	// Build chatter is diagnostic, not harness output — keep it off stdout so a
	// caller parsing stdout (e.g. --dry-run) stays clean.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Preflight verifies the harness can actually launch a sandbox before it commits
// to a run (claiming the ticket, mutating Linear state) — first that the Docker
// daemon is reachable, then that the sandbox image is present locally, building
// it on miss. A bare missing image otherwise surfaces only as exit 125 *after*
// the ticket is already claimed with no worktree (BEH-316); and because every
// session runs `docker run --rm`, the image is left unreferenced between runs,
// so a `docker system prune -a` (or Docker Desktop's "reclaim disk") quietly
// deletes it — build-on-miss makes that self-healing instead of a hard failure.
// run is injected so the check can be unit-tested; build is the (injected)
// image builder. Production passes ProbeRunner and BuildImage.
func Preflight(image, buildContext string, run func(name string, args ...string) ([]byte, error), build func(image, buildContext string) error) error {
	// Daemon reachable? `docker info` is the cheapest call that actually
	// round-trips to the daemon — `docker --version` is client-only and passes
	// even when the daemon is down.
	if out, err := run("docker", "info"); err != nil {
		return fmt.Errorf(
			"docker daemon not reachable — is Docker Desktop running? (%s)", reasonOr(out, err),
		)
	}

	// Image present? A missing image makes `docker run` try to pull a private/
	// nonexistent repo and die with exit 125. Build it instead of failing.
	if _, err := run("docker", "image", "inspect", image); err != nil {
		if berr := build(image, buildContext); berr != nil {
			return fmt.Errorf(
				"sandbox image %q missing and the build failed — run `docker build -t %s %s` manually (%s)",
				image, image, buildContext, berr,
			)
		}
		// Re-verify: a build that "succeeded" but produced no such tag (wrong
		// context, bad Dockerfile) would otherwise still die at `docker run`.
		if out, err := run("docker", "image", "inspect", image); err != nil {
			return fmt.Errorf(
				"sandbox image %q still not present after build — check the Dockerfile in %s (%s)",
				image, buildContext, reasonOr(out, err),
			)
		}
	}

	return nil
}

// reasonOr returns docker's own error line from out, falling back to err's text
// when out carries nothing useful.
func reasonOr(out []byte, err error) string {
	if hint := DockerErrorReason(string(out)); hint != "" {
		return hint
	}
	return err.Error()
}

// DockerErrorReason extracts the most informative line from docker's stderr: the
// last non-blank line that isn't docker's generic "See '… --help'." trailer.
// Surfaced on the console — otherwise docker's reason is teed only to the
// transcript and the operator sees a bare exit code (BEH-316).
func DockerErrorReason(stderr string) string {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || helpTrailerRE.MatchString(t) {
			continue
		}
		return t
	}
	return ""
}
