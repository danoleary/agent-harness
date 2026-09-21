// Package loophost is the daemon's half of the host port: the production
// [loop.Host]. Where hostio.Real serves one Stage for one run of one ticket, a
// Real here outlives every ticket the daemon works — it owns the stop sentinel on
// disk, the tracker queue, the per-ticket pipeline run, and the between-ticket
// disk reclaim (ADR-0002: every operation that leaves the process is host-side).
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
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/pipeline"
	"github.com/danoleary/agent-harness/internal/proc"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/ship"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/tracker"
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

// Real is the production [loop.Host].
type Real struct {
	cfg    config.Config
	log    loop.Narrator
	client tracker.Tracker

	// stopFile is the absolute path of the STOP sentinel: one of the daemon's two
	// stop signals, and the only one an operator can raise from another shell.
	stopFile string
	// sigStop is the other: flipped by the first SIGINT. An atomic.Bool is the seam
	// between the async signal goroutine and the synchronous StopRequested check.
	sigStop atomic.Bool
}

var _ loop.Host = (*Real)(nil)

// New builds the daemon's host. cfg.StopFile is resolved here, once: a relative
// override hangs off the Consumer checkout (so the default ".agent-harness/STOP"
// lands where CONSUMER.md documents it), an absolute one is taken as given.
func New(cfg config.Config, log loop.Narrator, client tracker.Tracker) *Real {
	stopFile := cfg.StopFile
	if !filepath.IsAbs(stopFile) {
		stopFile = filepath.Join(cfg.ProjectPath, stopFile)
	}
	return &Real{cfg: cfg, log: log, client: client, stopFile: stopFile}
}

// --- stop control -----------------------------------------------------------

// ClearStopFile deletes the sentinel, treating an already-absent file as success —
// the startup clear must not fail merely because there was nothing to clear.
func (h *Real) ClearStopFile() error {
	err := os.Remove(h.stopFile)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// StopRequested folds the two stop signals into one predicate (DESIGN.md "Two
// signals, one check"). A stat error other than not-exist reads as "present", so
// an unreadable sentinel errs toward stopping rather than ignoring an operator's
// `touch`.
func (h *Real) StopRequested() bool {
	if h.sigStop.Load() {
		return true
	}
	_, err := os.Stat(h.stopFile)
	return err == nil || !os.IsNotExist(err)
}

// RequestStop raises the in-process stop signal. The SIGINT handler calls it; the
// loop observes it at its next between-ticket (or between-tick) checkpoint.
func (h *Real) RequestStop() { h.sigStop.Store(true) }

// StopFile is where the sentinel lives, for the entrypoint's own narration.
func (h *Real) StopFile() string { return h.stopFile }

// --- queue ------------------------------------------------------------------

// checkout binds the Consumer's checkout and branch prefix to the git seam once,
// so the daemon's two git reads name the ticket and nothing else.
func (h *Real) checkout() gitpkg.Checkout {
	return gitpkg.Open(h.cfg.ProjectPath, h.cfg.BranchPrefix)
}

func (h *Real) FetchMain() error { return h.checkout().FetchMain() }

// ResolveNext selects and claims the top-of-queue ticket. It is always a real run,
// never a dry-run: the loop claims-on-select (ADR-0003) so a concurrent selection
// can't grab the same ticket. A selection/claim failure is folded into "no ticket"
// — the daemon idles and re-polls rather than crashing on one bad poll.
func (h *Real) ResolveNext() (string, bool) {
	sel := pipeline.ResolveNext(h.client, false, h.log)
	if !sel.Proceed {
		return "", false
	}
	return sel.Identifier, true
}

func (h *Real) ReleaseTicket(id string) error { return h.client.ReleaseToTodo(id) }

// CloseTicket consumes a recommend-close verdict by moving the superseded ticket to
// the terminal Canceled state (BEH-682), so it exits the selection + reaper pools
// for good rather than re-looping to the same "nothing to ship" conclusion.
func (h *Real) CloseTicket(id string) error { return h.client.MoveToCanceled(id) }

func (h *Real) CommentTicket(id, body string) error { return h.client.AddComment(id, body) }

// ListInProgressClaims projects the tracker's agent-claimed In Progress set onto
// the loop's StaleClaim shape (BEH-677), so the daemon stays decoupled from the
// Tracker port.
func (h *Real) ListInProgressClaims() ([]loop.StaleClaim, error) {
	claims, err := h.client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	out := make([]loop.StaleClaim, 0, len(claims))
	for _, c := range claims {
		out = append(out, loop.StaleClaim{
			Identifier:  c.Identifier,
			StartedAt:   c.StartedAt,
			HasLinkedPR: c.HasLinkedPR,
		})
	}
	return out, nil
}

// TicketHasRemoteBranch reads origin's heads for a branch naming the ticket. It
// fails SAFE toward true (see gitpkg): a flaky read must never cost a live claim.
func (h *Real) TicketHasRemoteBranch(id string) bool {
	return h.checkout().TicketHasRemoteBranch(id)
}

// --- the work ---------------------------------------------------------------

// RunPipeline runs the full implementation → review → retrospective chain over one
// already-claimed ticket, mirroring cmd/pipeline's wiring: its own ticket-keyed
// runlog and run id (the daemon runs many tickets, so each gets its own log dir).
// The stage args carry PreClaimed=true because ResolveNext claimed the ticket on
// selection, so the implementation stage skips its own claim and releases on a
// preflight failure (ADR-0003).
func (h *Real) RunPipeline(identifier string) stages.Result {
	cfg, log, runID, err := stages.Setup(identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		// A setup failure produced no PR; surface it as a plain no-PR outcome so the
		// breaker counts it like any other failure to ship.
		return stages.Result{}
	}
	args := stages.Args{Identifier: identifier, PreClaimed: true}
	// One Host for the whole slice: the three stages share the run id it stamps into
	// every container name and transcript, and the tracker client it resolves once.
	host := hostio.New(cfg, log, runID, args.Verbose)
	return pipeline.Run(pipeline.Deps{
		FetchMain:      host.FetchMain,
		Implementation: func() stages.Result { return stages.Implementation(host, cfg, log, args) },
		Review:         func() stages.Result { return stages.Review(host, cfg, log, args) },
		Retrospective:  func() stages.Result { return stages.Retrospective(host, cfg, log, args) },
		Log:            log,
	})
}

// RecoverCommittedFix finishes a branch a previous run left committed, gate-green
// and unpushed (BEH-713). The policy is ship.Recover — the same host-side "finish a
// branch" the review stage ends on — run over a Host bound to this ticket. A setup
// failure means there is nothing to recover: fall through to the normal release.
func (h *Real) RecoverCommittedFix(identifier string) (stages.Result, bool) {
	cfg, log, runID, err := stages.Setup(identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}, false
	}
	rec := ship.Recover(hostio.New(cfg, log, runID, false), h.log, identifier)
	out := stages.Result{}
	if rec.Shipped {
		out = stages.Result{OK: true, Disposition: stages.Shipped}
	}
	return out, rec.Attempted
}

// --- disk reclaim (ADR-0005) ------------------------------------------------

// FreeDisk statfs's PROJECT_PATH: always present, and on the same volume as the
// worktrees under it, so it is the cheap gate for the whole reclaim ladder.
func (h *Real) FreeDisk() (uint64, error) { return sandbox.FreeDiskBytes(h.cfg.ProjectPath) }

// PruneMergedWorktrees shells out to the checkout's prune-merged-worktrees.sh
// --yes, the existing, tested, squash-merge-aware reclaimer. It runs with the
// Consumer checkout as its cwd so the script's `git rev-parse --show-toplevel`
// resolves there. The merged/clean classification stays entirely in the script
// (single source of truth); here we only parse how many it removed for the
// daemon's narration. A non-zero exit (e.g. gh unreachable) surfaces as an error
// the loop logs and swallows — reclaim is never a ticket outcome.
func (h *Real) PruneMergedWorktrees() (int, error) {
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
func (h *Real) CachePrune() error {
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
func (h *Real) DockerPrune() error {
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
func (h *Real) KillContainers() {
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
