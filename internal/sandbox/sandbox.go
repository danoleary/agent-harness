// Package sandbox builds the docker argv to run one sandboxed claude session.
package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/beherd/agent-harness/internal/git"
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

// ExitOOMKill is the exit code of a container killed by SIGKILL (128 + 9). Under
// host memory pressure this is the Linux OOM-killer reaping the container (or a
// `docker kill`). Unlike a non-zero code the process itself returns — a real
// failure such as a typecheck error or a failing test — a 137 is environmental
// and transient: the same `pnpm install --frozen-lockfile` that 137'd under
// memory pressure completed in ~4s on a bare retry once memory freed (BEH-524).
// So a 137 is worth retrying; a genuine failure is not.
const ExitOOMKill = 137

// transientStartReasonRE matches the docker stderr reasons for the environmental,
// transient class of exit-125 launch failure. Two known signatures, both the host
// momentarily wedging rather than a code/config fault:
//   - the overlay2 store gone read-only under host disk/IO pressure — the kernel's
//     EROFS ("read-only file system"), e.g. `driver "overlay2" failed to remove
//     root filesystem: unlinkat …: read-only file system` (BEH-542).
//   - the container vanishing mid-launch with the engine's wait failing on a
//     severed stream — `error waiting for container: unexpected EOF` (BEH-547/
//     BEH-550: a session was killed ~90s in by exactly this, costing the whole
//     ticket; the same memory-pressure/OOM class as the 137 kill, but it took out
//     the whole container rather than one command, so docker reports it as a
//     launch failure with no recovery turn left to the agent). The match is
//     anchored on "waiting for container" so a bare "unexpected EOF" from a
//     genuine config/parse fault (e.g. a yaml parse error) stays terminal.
//
// Like the 137 OOM-kill (ExitOOMKill), the same `docker run` succeeds on a bare
// retry once the engine recovers — so both are worth retrying.
var transientStartReasonRE = regexp.MustCompile(`(?i)read-only file system|waiting for container: unexpected EOF`)

// IsRetryableStartFailure reports whether a docker "cannot start" (exit 125)
// reason line is one of the transient engine-wedge classes worth retrying
// (overlay2 read-only filesystem, or a container-wait "unexpected EOF"), mirroring
// the ExitOOMKill (137) precedent. reason is docker's own error line
// (DockerErrorReason). An empty or unrecognised reason is NOT retryable: only the
// known transient signatures match, so a genuine 125 (daemon down, image missing,
// bad flag) still fails fast and terminal.
func IsRetryableStartFailure(reason string) bool {
	return transientStartReasonRE.MatchString(reason)
}

// MinFreeDiskBytes is the host free-space floor Preflight refuses to launch below.
// Below it a session reliably exhausts the disk mid-run — a fresh worktree's
// web/node_modules (multiple GiB) plus the container's overlay2 layers plus
// logs/findings outgrow what's left — and the failure then surfaces opaquely and
// *after* the ticket is already claimed: an overlay2 "read-only file system"
// exit-125 teardown (BEH-542) or a findings-dir mkdir ENOSPC (BEH-540). Failing
// fast here turns that into one clear, actionable message before any state is
// mutated. The floor sits well above new-worktree.sh's 2 GiB *warning* threshold,
// which proved too low — BEH-540's session was warned at 1282 MiB free and still
// ran and died; the harness needs headroom for a whole worktree + overlay churn,
// not just a warning's-worth.
const MinFreeDiskBytes = 5 << 30 // 5 GiB

// DiskReclaimHint is the single source of truth for the "how to free disk"
// remediation appended to every disk-full message (the Preflight floor error
// here, plus the stages' findings-dir-mkdir ENOSPC warnings). One constant so
// the three sites can never drift (BEH-566).
//
// It names the Docker reclaims FIRST because they are usually the biggest win
// for the harness: it launches `docker run` sandboxes, so stale build cache and
// unreferenced images accrete on the Docker volume and are frequently the
// largest consumer — yet a real incident (BEH-433, 3.6 GiB free) showed the
// pnpm/worktree reclaims recovered almost nothing while `docker builder prune
// -af` recovered 6.6 GB. The pnpm store + merged-worktree prunes follow as the
// cheaper, non-destructive fallbacks. (Worktree node_modules are hardlinks into
// the pnpm store, so pruning worktrees only orphans store content — a follow-up
// `pnpm store prune` is what actually reclaims it.)
const DiskReclaimHint = "reclaim Docker space with `docker builder prune -af` / `docker system prune -af` " +
	"(build cache and unreferenced images are often the largest consumer for the harness), " +
	"or free space with `pnpm store prune` and by pruning merged worktrees " +
	"(`scripts/prune-merged-worktrees.sh --yes`)"

// FreeDiskBytes reports the bytes available to an unprivileged writer on the
// filesystem holding path, via statfs (Bavail × Bsize). It is the production
// diskFree passed to Preflight; tests inject their own.
func FreeDiskBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// FindingsMountPath is the fixed container path the findings dropbox is mounted at.
const FindingsMountPath = "/findings"

// PnpmStoreMountPath is the fixed container path the persistent pnpm store is mounted at.
const PnpmStoreMountPath = "/pnpm-store"

// bashDefaultTimeoutMS / bashMaxTimeoutMS raise Claude Code's per-Bash-command
// deadline inside the agent container. The agent's first act every session is
// `new-worktree.sh`, whose 1428-file `git worktree add` checkout + frozen install
// + Playwright Chromium runs for minutes on slow sandbox I/O — past the CLI's
// short default (2 min documented; this sandbox historically hit a ~3m20s wall
// per BEH-480), which fired mid-checkout and burned a turn on the re-run
// (BEH-486, follow-up to BEH-480). 10 min comfortably covers setup in one shot;
// the 20 min ceiling lets the agent set an even longer explicit deadline (a full
// `build`/`test-storybook`) while staying under the session wall-clock cap.
const (
	bashDefaultTimeoutMS = "600000"  // 10 minutes
	bashMaxTimeoutMS     = "1200000" // 20 minutes
)

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

	// Give the agent's Bash tool a generous per-command deadline so the first-run
	// `new-worktree.sh` (multi-minute checkout + install on slow I/O) completes in
	// one shot instead of hitting the CLI's short default mid-checkout (BEH-486).
	// Not secrets — passed by value.
	args = append(args,
		"-e", "BASH_DEFAULT_TIMEOUT_MS="+bashDefaultTimeoutMS,
		"-e", "BASH_MAX_TIMEOUT_MS="+bashMaxTimeoutMS,
	)

	// Stamp the harness bot identity on the agent's in-container commits (the
	// handoff commit, any checkpoint). GIT_AUTHOR_*/GIT_COMMITTER_* env take
	// precedence over every git config level, so they override the bind-mounted
	// checkout's placeholder `Test <test@example.com>` LOCAL config that defeats
	// the entrypoint's `git config --global` identity (BEH-579). Not secrets —
	// passed by value.
	args = append(args,
		"-e", "GIT_AUTHOR_NAME="+git.HarnessAuthorName,
		"-e", "GIT_AUTHOR_EMAIL="+git.HarnessAuthorEmail,
		"-e", "GIT_COMMITTER_NAME="+git.HarnessAuthorName,
		"-e", "GIT_COMMITTER_EMAIL="+git.HarnessAuthorEmail,
	)

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

// installCommand is the pre-session prep the review tool runs to repopulate the
// worktree's web/node_modules — stripped on handoff (BEH-412) — against the warm
// pnpm store, mirroring new-worktree.sh. A frozen install only, so the cold review
// SESSION finds node_modules present instead of paying it mid-gate (BEH-490). Run
// from `web/`, the source of truth for the pnpm scripts (see herd CLAUDE.md).
const installCommand = "cd web && pnpm install --frozen-lockfile"

// gateCommand is the gate the harness re-runs as ground truth: a fresh install
// (against the warm pnpm store) then the repo's own `check` + `typecheck`.
//
// It runs `typecheck` (tsgo --noEmit), NOT the full `pnpm run build`. The build's
// vite bundling + prerender crawl is memory-heavy enough to be OOM-killed (exit
// 137) in the sandbox even on a correct diff — and the OOM is not confined to the
// prerender step, so the baked-in HERD_SANDBOX=1 (which skips prerender) does not
// save it. That once killed a green, reviewed branch out of shipping (BEH-529;
// same class as BEH-407/477/491/519). `typecheck` is the accepted in-sandbox diff-
// validation signal; CI's full `build` is the SSR-shell backstop (the harness
// watches CI post-PR via ci.WatchAndFix).
const gateCommand = installCommand + " && pnpm run check && pnpm run typecheck"

// BuildGateRunArgs builds the argv (everything after `docker`) for the throwaway
// container that re-runs `pnpm check && pnpm typecheck` on the reviewed branch. This
// is the harness's OWN ground truth — never the agent's self-report — and the
// push gate (DESIGN.md). The container carries NO secrets at all (not even the
// Claude credential): it runs no model, only the gates, so nothing needs to cross
// the boundary. It runs in the worktree (the branch under review), not the main
// checkout.
func BuildGateRunArgs(c GateConfig) []string {
	return buildWorktreeBashArgs(c, gateCommand)
}

// BuildInstallRunArgs builds the argv for the throwaway container that pre-populates
// the worktree's web/node_modules before the cold review session. The implementation
// tool strips node_modules on handoff (BEH-412) so a non-Linux host reviewer installs
// fresh; in the always-Linux review sandbox that strip just means the session would
// otherwise pay `pnpm install` mid-gate (BEH-490). This runs the install up front —
// install ONLY (the later gate container owns check/build) — so the session opens
// onto a ready worktree. Like the gate it carries NO secrets and runs in the worktree.
func BuildInstallRunArgs(c GateConfig) []string {
	return buildWorktreeBashArgs(c, installCommand)
}

// buildWorktreeBashArgs is the shared skeleton for the throwaway worktree containers
// (install prep + ground-truth gate): a secret-free container that bind-mounts the
// checkout and the warm pnpm store and runs `command` in the worktree via bash.
func buildWorktreeBashArgs(c GateConfig, command string) []string {
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
		"bash", "-lc", command,
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
	cmd := exec.Command("docker", "build", "-t", image, buildContext) // allow-unbounded-exec: docker build can take minutes, streams user-visible progress
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
// image builder, diskFree the (injected) free-space probe. Production passes
// ProbeRunner, BuildImage, and FreeDiskBytes.
func Preflight(image, buildContext string, run func(name string, args ...string) ([]byte, error), build func(image, buildContext string) error, diskFree func(path string) (uint64, error)) error {
	// Disk precondition FIRST — before any docker call. A full host disk is the
	// root cause that, downstream, wedges the daemon (overlay2 read-only, exit
	// 125) and fails the findings mkdir (ENOSPC) — opaquely, and after the ticket
	// is claimed (BEH-540). Refuse to launch with an actionable message instead. A
	// statfs error is itself non-fatal: don't block a launch because free space
	// couldn't be read — let the docker probes below run.
	if free, err := diskFree(buildContext); err == nil && free < MinFreeDiskBytes {
		return fmt.Errorf(
			"insufficient free disk to launch a sandbox: %d MiB free at %s, need ≥ %d MiB — %s and re-run",
			free>>20, buildContext, MinFreeDiskBytes>>20, DiskReclaimHint,
		)
	}

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
