// Package loophost holds the daemon's production adapters: the [loop.Host] that
// does its chores ([Daemon]), and the ticket loop's [loop.Source] and
// [loop.Pipeline] ([TicketSource], [TicketPipeline]) — ADR-0014, ADR-0019. Where
// hostio.Real serves one Stage for one run of one ticket, these outlive every item
// the daemon works: the stop sentinel on disk, the tracker queue, the per-ticket
// pipeline run, and the between-item disk reclaim (ADR-0002: every operation that
// leaves the process is host-side).
//
// It exists so `cmd/loop` can be what it claims to be — a thin composition root.
// The helpers here were 400 lines of `package main`: 11% covered, importable by
// no test, and among them the committed-fix recovery, sixty lines of policy
// deciding whether a ticket ships. They are ordinary internal code now, and the
// binary is the wiring that names them.
package loophost

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/proc"
	"github.com/danoleary/agent-harness/internal/sandbox"
)

// The disk-reclaim and hard-abort shell-outs are all bounded, so a stalled `gh`,
// a wedged package manager or an unresponsive Docker daemon can never hang the
// between-ticket chores or the exit path (BEH-388, ADR-0005).
const (
	// pruneTimeout bounds the merged-worktree prune, which makes one `gh pr view`
	// per worktree — hence the more generous cap.
	pruneTimeout = 5 * time.Minute
	// cachePruneTimeout bounds the Consumer's declared cache-reclaim command.
	cachePruneTimeout = 2 * time.Minute
	// dockerPruneTimeout bounds the Docker build-cache/image prune: a large build
	// cache takes a while to walk and delete.
	dockerPruneTimeout = 5 * time.Minute
	// killDockerTimeout bounds the hard-abort docker calls so a wedged daemon can't
	// hang the exit path — the second Ctrl-C must always terminate promptly.
	killDockerTimeout = 10 * time.Second
)

// Daemon is the production [loop.Host]: the chores every loop shares, whatever its
// Source. Nothing here knows about tickets.
type Daemon struct {
	cfg config.Config
	// checkToken proves GH_TOKEN is accepted. A field so a test can stand in for
	// the network; NewDaemon wires the real GitHub probe.
	checkToken func() error
	// checkSource proves the Source's own credential works (for tickets,
	// TicketSource.CheckAccess). Run after checkToken; nil means the Source has
	// no credential of its own.
	checkSource func() error

	// stopFile is the absolute path of the plain STOP sentinel, which stops every
	// loop, and loopStopFile the loop's own (STOP-<name>). A sentinel is one of
	// the daemon's two stop signals, and the only one an operator can raise from
	// another shell.
	stopFile     string
	loopStopFile string
	// sigStop is the other: flipped by the first SIGINT. An atomic.Bool is the seam
	// between the async signal goroutine and the synchronous StopRequested check.
	sigStop atomic.Bool
}

var _ loop.Host = (*Daemon)(nil)

// NewDaemon builds the host for loop l. checkSource is the Source's startup
// access probe, which CheckAccess runs after the GH_TOKEN check. The sentinels are
// resolved here, once: a relative path hangs off the Consumer checkout (so the
// default ".agent-harness/STOP" lands where CONSUMER.md documents it), an absolute
// STOP_FILE is taken as given.
func NewDaemon(cfg config.Config, l config.Loop, checkSource func() error) *Daemon {
	resolve := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(cfg.ProjectPath, path)
	}
	return &Daemon{
		cfg:          cfg,
		checkToken:   func() error { return github.CheckToken(github.NewTransport(cfg.GitHubToken)) },
		checkSource:  checkSource,
		stopFile:     resolve(cfg.StopFile),
		loopStopFile: resolve(l.StopFile()),
	}
}

// --- stop control -----------------------------------------------------------

// ClearStopFile deletes both sentinels, treating an already-absent file as
// success — the startup clear must not fail merely because there was nothing to
// clear.
func (h *Daemon) ClearStopFile() error {
	for _, path := range []string{h.stopFile, h.loopStopFile} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// StopRequested folds the two stop signals into one predicate (DESIGN.md "Two
// signals, one check"). A stat error other than not-exist reads as "present", so
// an unreadable sentinel errs toward stopping rather than ignoring an operator's
// `touch`.
func (h *Daemon) StopRequested() bool {
	if h.sigStop.Load() {
		return true
	}
	for _, path := range []string{h.stopFile, h.loopStopFile} {
		if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
			return true
		}
	}
	return false
}

// RequestStop raises the in-process stop signal. The SIGINT handler calls it; the
// loop observes it at its next between-item (or between-tick) checkpoint.
func (h *Daemon) RequestStop() { h.sigStop.Store(true) }

// StopFile is where the plain sentinel lives, which stops every loop.
func (h *Daemon) StopFile() string { return h.stopFile }

// LoopStopFile is where this loop's own sentinel lives.
func (h *Daemon) LoopStopFile() string { return h.loopStopFile }

// --- startup -----------------------------------------------------------------

// checkout binds the Consumer's checkout and branch prefix to the git seam once.
func (h *Daemon) checkout() gitpkg.Checkout {
	return gitpkg.Open(h.cfg.ProjectPath, h.cfg.BranchPrefix)
}

func (h *Daemon) FetchMain() error { return h.checkout().FetchMain() }

// CheckAccess proves both host credentials work before the daemon polls: GH_TOKEN
// (every Consumer pushes and opens PRs with it, whatever its Source) and the
// Source's own, via the probe the composition root handed [NewDaemon].
func (h *Daemon) CheckAccess() error {
	if err := h.checkToken(); err != nil {
		return err
	}
	if h.checkSource == nil {
		return nil
	}
	return h.checkSource()
}

// --- disk reclaim (ADR-0005) ------------------------------------------------

// FreeDisk statfs's PROJECT_PATH: always present, and on the same volume as the
// worktrees under it, so it is the cheap gate for the whole reclaim ladder.
func (h *Daemon) FreeDisk() (uint64, error) { return sandbox.FreeDiskBytes(h.cfg.ProjectPath) }

// PruneMergedWorktrees shells out to the checkout's prune-merged-worktrees.sh
// --yes, the existing, tested, squash-merge-aware reclaimer. It runs with the
// Consumer checkout as its cwd so the script's `git rev-parse --show-toplevel`
// resolves there. The merged/clean classification stays entirely in the script
// (single source of truth); here we only parse how many it removed for the
// daemon's narration. A non-zero exit (e.g. gh unreachable) surfaces as an error
// the loop logs and swallows — reclaim is never a ticket outcome.
func (h *Daemon) PruneMergedWorktrees() (int, error) {
	script := filepath.Join(h.cfg.ProjectPath, "scripts", "prune-merged-worktrees.sh")
	out, err := proc.CombinedOutputInDir(pruneTimeout, h.cfg.ProjectPath, script, "--yes")
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return parsePrunedCount(string(out)), nil
}

// CachePrune runs the Consumer's declared `cache.prune_command` — the cheap,
// non-destructive, network-free secondary reclaim. A Consumer that declares none
// makes this a no-op rather than running someone else's package manager: the rung
// is skipped, not faked.
//
// Shell-interpreted (`sh -c`) because a Consumer writes a command line, not an
// argv, and run on the HOST rather than in a sandbox: the caches it reclaims are
// the host's, the same reason the worktree and Docker prunes run host-side. The
// checkout anchors the call; a toolchain whose cache is global ignores cwd.
func (h *Daemon) CachePrune() error {
	if h.cfg.CachePruneCommand == "" {
		return nil
	}
	out, err := proc.CombinedOutputInDir(cachePruneTimeout, h.cfg.ProjectPath, "sh", "-c", h.cfg.CachePruneCommand)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DockerPrune reclaims the harness's usual disk hog — Docker build cache and
// unreferenced images. `builder prune -f` clears the build cache; `image prune -af`
// removes all dangling AND unreferenced images, including the between-run sandbox
// image (`docker run --rm` leaves it unreferenced) — which self-heals, since
// Preflight rebuilds or pulls it on the next launch (resolve-on-miss). Both are
// `-f` so they never block on a confirmation prompt. The `builder` failure is
// returned first; the image prune still runs.
func (h *Daemon) DockerPrune() error {
	builderOut, builderErr := proc.CombinedOutputInDir(dockerPruneTimeout, h.cfg.ProjectPath, "docker", "builder", "prune", "-f")
	imageOut, imageErr := proc.CombinedOutputInDir(dockerPruneTimeout, h.cfg.ProjectPath, "docker", "image", "prune", "-af")
	if builderErr != nil {
		return fmt.Errorf("docker builder prune: %w: %s", builderErr, strings.TrimSpace(string(builderOut)))
	}
	if imageErr != nil {
		return fmt.Errorf("docker image prune: %w: %s", imageErr, strings.TrimSpace(string(imageOut)))
	}
	return nil
}

// parsePrunedCount reads the removed-worktree count from the prune script's
// authoritative summary line ("Pruned N worktree(s)."), degrading to 0 when the
// script removed nothing or printed no summary. Parsing the script's own count
// keeps a single source of truth for "what got removed".
func parsePrunedCount(output string) int {
	const prefix = "Pruned "
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(fields) > 0 {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				return n
			}
		}
	}
	return 0
}

// --- hard abort --------------------------------------------------------------

// KillContainers best-effort kills any container whose name carries THIS
// Consumer's harness prefix, so a hard abort (the second Ctrl-C) tears down the
// in-flight session rather than orphaning it. The active session container is
// detached in its own process group — the parent's SIGINT never reached it — so
// naming the kill by prefix is how the host reaches across that boundary.
//
// The prefix is Consumer-scoped (sandbox.ContainerPrefix), not a constant: one
// host can run the harness against several projects at once, and a shared prefix
// would make Ctrl-C here kill another project's in-flight session. Failures are
// ignored — the process is exiting.
func (h *Daemon) KillContainers() {
	out, _, err := proc.Output(killDockerTimeout, "docker", "ps", "-q", "--filter", "name="+sandbox.ContainerPrefix(h.cfg.ProjectPath))
	if err != nil || len(out) == 0 {
		return
	}
	for _, id := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if id = strings.TrimSpace(id); id != "" {
			_ = proc.Run(killDockerTimeout, "docker", "kill", id)
		}
	}
}
