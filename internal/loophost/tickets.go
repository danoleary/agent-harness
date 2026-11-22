package loophost

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/hostlock"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/overlap"
	"github.com/danoleary/agent-harness/internal/pipeline"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/ship"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// TicketSource is the ticket loop's [loop.Source]: the Tracker's ready queue, the
// agent-claimed In Progress set the reaper sweeps, and the ticket comments the
// daemon leaves as breadcrumbs (ADR-0019).
type TicketSource struct {
	cfg    config.Config
	log    loop.Narrator
	client tracker.Tracker
	// prs, when set, are the open PRs selection screens a ticket against: a ticket
	// that names files the harness's own PRs already change waits (#97).
	prs OpenPRs
	// screen is the selection's screens, built once so a deferral or skip that
	// stands poll after poll reaches the loop stream once (#281).
	screen pipeline.NextResolver
	// held is why the last poll was held rather than empty ([loop.Holder]).
	held string
}

// OpenPRs is the repository's open PRs and the files each changes: the GitHub
// read the overlap check needs ([github.Pulls]).
type OpenPRs interface {
	OpenPRs() ([]github.PullRequest, error)
	Files(number int) ([]string, error)
}

var (
	_ loop.Source = (*TicketSource)(nil)
	_ loop.Holder = (*TicketSource)(nil)
)

// NewTicketSource builds the ticket Source over the Consumer's Tracker.
func NewTicketSource(cfg config.Config, log loop.Narrator, client tracker.Tracker) *TicketSource {
	return &TicketSource{cfg: cfg, log: log, client: client}
}

// RepoPRs reads the open PRs of the repository the Consumer's checkout pushes to,
// through gh, which fills the owner and repo from the checkout.
func RepoPRs(cfg config.Config) *github.Pulls {
	return github.NewPulls(github.NewGhTransport(cfg.ProjectPath), "{owner}/{repo}")
}

// WatchPRs turns on the overlap check (#97, ADR-0032): selection defers a ticket
// whose title or body names files an open harness PR, or a claimed ticket's
// worktree, already changes. OVERLAP_THRESHOLD=0 leaves it off.
func (s *TicketSource) WatchPRs(prs OpenPRs) *TicketSource {
	s.prs, s.screen = prs, nil
	return s
}

// checkout binds the Consumer's checkout and branch prefix to the git seam once,
// so the source's git reads name the ticket and nothing else.
func (s *TicketSource) checkout() gitpkg.Checkout {
	return gitpkg.Open(s.cfg.ProjectPath, s.cfg.BranchPrefix)
}

// freshMain is origin/main after a best-effort fetch. The loop fetches only at
// startup and after a ticket, so a queue the no-work record holds would otherwise
// never see main move and pass over the ticket until a restart (#330). The screen
// reads it only when the offered ticket has a record, so a poll without one makes
// no network call.
func (s *TicketSource) freshMain() (string, error) {
	if err := s.checkout().FetchMain(); err != nil {
		s.log.Event("⚠ could not fetch origin/main to check a ticket whose last run found no work (" + err.Error() + ") — comparing against the last fetch")
	}
	return s.checkout().MainSHA()
}

// CheckAccess proves the tracker's credential works via one side-effect-free
// selection read — the same call every poll makes, so passing here means polls
// can succeed. The daemon runs it at startup, after the GH_TOKEN check
// ([NewDaemon]).
func (s *TicketSource) CheckAccess() error {
	if _, _, err := s.client.SelectNextTicket(); err != nil {
		return fmt.Errorf("the %s tracker rejected a queue read — check its credentials and the tracker config: %w", s.cfg.Tracker.Kind, err)
	}
	return nil
}

// Next selects and claims the top-of-queue ticket. It is always a real run, never
// a dry-run: the loop claims-on-select (ADR-0003), and the claim is exclusive
// under the Consumer's claim lock, so another loop instance selecting at the same
// moment can't grab the same ticket (#89). A selection/claim failure comes back as err, which the
// daemon narrates as a failed read — not an empty queue — before re-polling. The
// item's Branch is the slugged feature branch the Stages will create and push, and
// its Context the ticket as selection read it (a [nowork.Fingerprint], #330).
func (s *TicketSource) Next() (loop.WorkItem, *lease.Lease, bool, error) {
	sel := pipeline.ResolveNext(s.Resolver(), hostlock.File(config.ClaimLockFile(s.cfg.ProjectPath)), false, s.cfg.Tracker.QueueHint(), s.log)
	s.held = sel.Held
	if sel.Err != nil {
		return loop.WorkItem{}, nil, false, sel.Err
	}
	if !sel.Proceed {
		return loop.WorkItem{}, nil, false, nil
	}
	key := sel.Lease.Key()
	item := loop.WorkItem{Key: key, Branch: s.checkout().Worktree(ticket.Slug(key)).Branch(), Context: sel.Fingerprint}
	return item, sel.Lease, true, nil
}

// Resolver is the tracker selection Next claims through: screened against the
// runs that are live (#366), against the record of runs that found nothing to
// hand off (#330), and against the work in flight when the overlap check is on.
// `pipeline --next` selects through it too, so a single-shot run passes over and
// defers what the loop would.
func (s *TicketSource) Resolver() pipeline.NextResolver {
	if s.screen == nil {
		s.screen = pipeline.SkipLive(s.client, s.runLive, s.log)
		s.screen = pipeline.SkipNoWork(s.screen, nowork.For(s.cfg), s.freshMain, s.log)
		if s.prs != nil {
			policy := overlap.Policy{Threshold: s.cfg.OverlapThreshold, EscapeLabel: s.cfg.OverlapEscapeLabel}
			branch := func(key string) string { return s.checkout().Worktree(ticket.Slug(key)).Branch() }
			s.screen = pipeline.DeferOverlaps(s.screen, s.inFlight, branch, policy, s.log)
		}
	}
	return s.screen
}

// runLive reads whether a run working key is live: it holds the ticket's run
// lock ([HoldRun]).
func (s *TicketSource) runLive(key string) (bool, error) {
	return hostlock.File(config.RunLockFile(s.cfg.ProjectPath, ticket.Slug(key))).Held()
}

// Held says why the last Next found nothing to claim over a queue that was not
// empty: the overlap screen deferred every ready ticket (#281). "" is an empty
// queue.
func (s *TicketSource) Held() string { return s.held }

// inFlight is the work a ready ticket may collide with (#97): each open PR on the
// harness's branch prefix with the files it changes, then each claimed ticket's
// worktree with what it has changed so far — a ticket mid-run has no PR yet — and
// the tracked files its own title and body name. A ticket another loop instance
// claimed a moment ago has changed nothing yet, and a sibling naming the same
// module must still wait for it. A human's branch or a fork is not the harness's
// work, and two PRs the harness did not open conflict no more often for it than
// they did before. Any failed read fails the whole read, which the screen
// narrates and dispatches through.
func (s *TicketSource) inFlight() ([]overlap.Work, error) {
	prs, err := s.prs.OpenPRs()
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}
	var work []overlap.Work
	for _, pr := range prs {
		if pr.Fork || !strings.HasPrefix(pr.HeadBranch, s.cfg.BranchPrefix+"/") {
			continue
		}
		files, err := s.prs.Files(pr.Number)
		if err != nil {
			return nil, fmt.Errorf("list the files of PR #%d: %w", pr.Number, err)
		}
		work = append(work, overlap.Work{Label: fmt.Sprintf("PR #%d", pr.Number), Branch: pr.HeadBranch, Files: files})
	}
	claims, err := s.client.ListInProgressClaims()
	if err != nil {
		return nil, fmt.Errorf("list claimed tickets: %w", err)
	}
	var tracked []string
	if len(claims) > 0 {
		if tracked, err = s.checkout().TrackedFiles(); err != nil {
			return nil, fmt.Errorf("list the checkout's files: %w", err)
		}
	}
	for _, c := range claims {
		wt := s.checkout().Worktree(ticket.Slug(c.Identifier))
		files, err := wt.ChangedFiles()
		if err != nil {
			return nil, fmt.Errorf("read what %s has changed: %w", c.Identifier, err)
		}
		t, err := s.client.FetchTicket(c.Identifier)
		if err != nil {
			return nil, fmt.Errorf("read claimed %s: %w", c.Identifier, err)
		}
		files = append(files, overlap.Named(t, tracked)...)
		work = append(work, overlap.Work{Label: c.Identifier + " (claimed)", Branch: wt.Branch(), Files: files})
	}
	return work, nil
}

// Release returns a stale claim to Todo.
func (s *TicketSource) Release(key string) error { return s.client.ReleaseToTodo(key) }

// Block labels the ticket blocked for a human (#230).
func (s *TicketSource) Block(key string) error { return s.client.MarkBlocked(key) }

// Comment posts a breadcrumb on the ticket.
func (s *TicketSource) Comment(key, body string) error { return s.client.AddComment(key, body) }

// Releasing narrates a ticket going back to Todo.
func (s *TicketSource) Releasing(why loop.ReleaseReason, key string) string {
	switch why {
	case loop.ReleaseCapAbort:
		return "spending-cap abort on " + key + "; releasing to Todo"
	case loop.ReleaseStale:
		return "reaping stale claim " + key + " (In Progress past TTL with no branch/PR); releasing to Todo (BEH-677)"
	default:
		return key + " produced no PR; releasing back to Todo so it isn't stranded In Progress (BEH-590)"
	}
}

// Claims projects the tracker's agent-claimed In Progress set onto the loop's
// StaleClaim shape (BEH-677), so the daemon stays decoupled from the Tracker port.
// A claim whose run lock is held is Live, and the reaper leaves it however old it
// reads (#366); a run lock that cannot be read counts as live, so a failed read
// never reaps a live run's claim.
func (s *TicketSource) Claims() ([]loop.StaleClaim, error) {
	claims, err := s.client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	out := make([]loop.StaleClaim, 0, len(claims))
	for _, c := range claims {
		live, err := s.runLive(c.Identifier)
		out = append(out, loop.StaleClaim{
			Key:         c.Identifier,
			StartedAt:   c.StartedAt,
			HasLinkedPR: c.HasLinkedPR,
			Live:        live || err != nil,
		})
	}
	return out, nil
}

// HeldBranches maps each branch the ticket loop holds right now — an agent-claimed
// In Progress ticket's `<branch_prefix>/<slug>`, while a run working it is live —
// to the ticket's Key. The PR loop skips those branches: the lease is keyed on the
// branch, and a ticket mid-run owns its PR's branch (ADR-0019). A shipped ticket stays In Progress until its PR merges, so
// the claim alone does not say a run is live; its run lock does ([HoldRun]). A
// worktree is no signal: an older binary, a restart mid-watch or a failed removal
// leaves one behind with no run in it, and keying on it kept the PR loop off the
// branch forever (#231). Once the run ends the branch is free, so the PR loop can
// fix the ticket's red CI, which the ticket loop never watches (ADR-0063). A run lock that
// cannot be read fails the read, as the claim list does.
func (s *TicketSource) HeldBranches() (map[string]string, error) {
	claims, err := s.client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	held := make(map[string]string, len(claims))
	for _, c := range claims {
		slug := ticket.Slug(c.Identifier)
		live, err := s.runLive(c.Identifier)
		if err != nil {
			return nil, fmt.Errorf("read whether %s's run is live: %w", c.Identifier, err)
		}
		if live {
			held[s.checkout().Worktree(slug).Branch()] = c.Identifier
		}
	}
	return held, nil
}

// Worktrees is the checkout's worktrees as the PR loop reads and reclaims them,
// with each worktree's own run lock read for a live run in it (#384). The holds
// ([TicketSource.HeldBranches]) key on the tracker's claim list, and a live run
// whose ticket that list misses would otherwise have its clean, pushed worktree
// removed from under it the moment its PR went red. A ticket's worktree is
// `.claude/worktrees/<slug>` and its run lock `runs/<slug>.lock`, so the lock is
// named by the worktree's base name; a worktree no run ever locked reads as not
// live.
type Worktrees struct {
	gitpkg.Checkout
	projectPath string
}

// NewWorktrees is cfg's checkout's [Worktrees].
func NewWorktrees(cfg config.Config) Worktrees {
	return Worktrees{Checkout: gitpkg.Open(cfg.ProjectPath, cfg.BranchPrefix), projectPath: cfg.ProjectPath}
}

// RunLive says whether a live run holds the run lock of the worktree at path.
func (w Worktrees) RunLive(path string) (bool, error) {
	return hostlock.File(config.RunLockFile(w.projectPath, filepath.Base(filepath.Clean(path)))).Held()
}

// ErrRunLive is [HoldRun]'s refusal: another live run already holds the
// ticket's run lock.
var ErrRunLive = errors.New("another live run holds the ticket's run lock")

// HoldRun takes the run lock on key for the run runID about to work it, and
// returns the func that lets it go when the run ends. Every run of a ticket holds
// it — the loop's Pipeline and its committed-fix recovery, a single-shot
// `pipeline` and each standalone Stage — so another loop can tell a live run from
// a worktree a dead one left (#231). It is a gate as well as a signal: a run that
// cannot take it does not run (#366). Another live run holding it is
// [ErrRunLive], and two runs on one worktree would each see the other's edits,
// checkpoint them as their own and push a tree their gates never saw. A lock that
// cannot be taken at all is that error. Either way the run is narrated and
// launches nothing. A taken lock is retried for a moment first: the liveness
// probe ([hostlock.File.Held]) takes it for an instant, and losing that race must
// not cost the run. The lock file names runID, so a Stage's recovery checkpoint
// can tell its own run's lock from another's.
func HoldRun(cfg config.Config, key, runID string, log loop.Narrator) (release func(), err error) {
	lock := hostlock.File(config.RunLockFile(cfg.ProjectPath, ticket.Slug(key)))
	unlock, ok, err := lock.TryLockAs(runID)
	for try := 1; err == nil && !ok && try < runLockTries; try++ {
		time.Sleep(runLockRetry)
		unlock, ok, err = lock.TryLockAs(runID)
	}
	switch {
	case err != nil:
		log.Event("✗ could not take " + key + "'s run lock (" + err.Error() + ") — not running " + key + " without it (#366)")
		return nil, fmt.Errorf("take %s's run lock: %w", key, err)
	case !ok:
		log.Event("⏸ another live run holds " + key + "'s run lock — not starting a second run on its worktree (#366)")
		return nil, ErrRunLive
	}
	return unlock, nil
}

// underRunLock runs run while it holds key's run lock ([HoldRun]). A run another
// live run holds comes back RunLive, and one whose lock could not be taken the
// plain no-PR failure; run never starts in either case.
func underRunLock(cfg config.Config, key, runID string, log loop.Narrator, run func() stages.Result) stages.Result {
	release, err := HoldRun(cfg, key, runID, log)
	switch {
	case errors.Is(err, ErrRunLive):
		return stages.Result{Disposition: stages.RunLive}
	case err != nil:
		return stages.Result{Err: err}
	}
	defer release()
	return run()
}

// RunLock is [underRunLock] over cfg's checkout, narrated to log: the run lock
// the PR and security-alerts loops' runs hold, as a ticket's run does. Their
// worktree, `.claude/worktrees/<slug>`, is named by the item's key as a ticket's
// is, so the merged-worktree prune's shield and the PR loop's reclaim see it live
// for as long as the run is in it. A PR-loop run's retrospective runs after its
// push, and once that PR merges the worktree passes the prune's test for dead
// (#416, ADR-0068).
func RunLock(cfg config.Config, log loop.Narrator) loop.RunLock {
	return func(key, runID string, run func() stages.Result) stages.Result {
		return underRunLock(cfg, key, runID, log, run)
	}
}

// runLockTries and runLockRetry bound how long HoldRun waits out a held run lock:
// long past a liveness probe, short against a run that really holds it.
const (
	runLockTries = 10
	runLockRetry = 50 * time.Millisecond
)

// HasRemoteBranch reads origin's heads for a branch naming the ticket. It fails
// SAFE toward true (see gitpkg): a flaky read must never cost a live claim.
func (s *TicketSource) HasRemoteBranch(key string) bool {
	return s.checkout().TicketHasRemoteBranch(key)
}

// TicketPipeline is a ticket loop's [loop.Pipeline]: the implementation → review
// → retrospective Stages over one ticket, as the loop's Steps choose them, and the
// committed-fix recovery that finishes a branch a previous run left unpushed
// (ADR-0019).
type TicketPipeline struct {
	log  loop.Narrator
	loop config.Loop
}

var _ loop.Pipeline = (*TicketPipeline)(nil)

// NewTicketPipeline builds the ticket Pipeline for l; log is the daemon's
// narration sink, which the recovery narrates to.
func NewTicketPipeline(log loop.Narrator, l config.Loop) *TicketPipeline {
	return &TicketPipeline{log: log, loop: l}
}

// setup loads the config with the loop's prompt bodies in place, mints a run id,
// and opens the ticket's runlog under the loop's own logs dir, whose loop.jsonl the
// daemon narrates to as well.
func (p *TicketPipeline) setup(key string) (config.Config, *runlog.Logger, string, error) {
	cfg, err := stages.LoadConfig()
	if err != nil {
		return config.Config{}, nil, "", err
	}
	p.loop.ApplyPrompts(&cfg.Prompts)
	log, err := runlog.New(filepath.Join(cfg.ProjectPath, p.loop.LogsDir()), key)
	if err != nil {
		return cfg, nil, "", err
	}
	return cfg, log, runlog.MakeRunID(time.Now()), nil
}

// Run runs the loop's Stages over one already-claimed ticket, mirroring
// cmd/pipeline's wiring: its own ticket-keyed runlog and run id (the daemon runs
// many tickets, so each gets its own log dir). The stage args carry the lease the
// Source claimed on selection, so the implementation stage skips its own claim,
// releases on a preflight failure (ADR-0003), and re-claims on a retry after a
// release (#25).
func (p *TicketPipeline) Run(item loop.WorkItem, claim *lease.Lease) stages.Result {
	cfg, log, runID, err := p.setup(item.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		// A setup failure produced no PR; surface it as a plain no-PR outcome so the
		// breaker counts it like any other failure to ship.
		return stages.Result{}
	}
	return underRunLock(cfg, item.Key, runID, p.log, func() stages.Result {
		return pipeline.Run(ticketDeps(p.loop, cfg, log, runID, ticketArgs(item, claim)))
	})
}

// ticketArgs are the stage args for one claimed ticket. The ticket as
// selection read it goes to the implementation stage, which records it if the run
// finds nothing to hand off (#330).
func ticketArgs(item loop.WorkItem, claim *lease.Lease) stages.Args {
	selected, _ := item.Context.(nowork.Fingerprint)
	return stages.Args{Identifier: item.Key, Lease: claim, Selected: selected}
}

// ticketDeps binds the loop's Stages to one ticket through the composition root
// cmd/pipeline uses (pipeline.ForTicket). A loop without the retro step runs no
// retrospective.
func ticketDeps(l config.Loop, cfg config.Config, log *runlog.Logger, runID string, args stages.Args) pipeline.Deps {
	d := pipeline.ForTicket(cfg, log, runID, args)
	if !l.Has(config.StepRetro) {
		d.Retrospective = nil
	}
	return d
}

// Recover finishes a branch a previous run left committed, gate-green and
// unpushed (BEH-713). The policy is ship.Recover — the same host-side "finish a
// branch" the review stage ends on — run over a Host bound to this ticket, with the
// push gate the review stage last recorded in the ticket's log dir. A tip the review
// refused comes back NeedsHuman with the reason (#230). A setup failure means there
// is nothing to recover: fall through to the normal release. So is a run lock
// that cannot be taken; one another live run holds comes back RunLive, for the
// loop to leave the claim to that run (#366).
func (p *TicketPipeline) Recover(item loop.WorkItem) (stages.Result, bool) {
	cfg, log, runID, err := p.setup(item.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}, false
	}
	attempted := false
	res := underRunLock(cfg, item.Key, runID, p.log, func() stages.Result {
		rec := ship.Recover(hostio.New(cfg, log, runID, false, ticket.Slug(item.Key)), p.log, item.Key, stages.LastPushGate(log.Dir))
		attempted = rec.Attempted
		return recovered(rec)
	})
	return res, attempted || res.Disposition == stages.RunLive
}

// recovered maps a committed-fix recovery onto the Result the loop branches on.
func recovered(rec ship.Recovery) stages.Result {
	switch {
	case rec.Shipped:
		return stages.Result{OK: true, Disposition: stages.Shipped}
	case rec.Refused != "":
		return stages.Result{Disposition: stages.NeedsHuman, Refusal: rec.Refused}
	default:
		return stages.Result{}
	}
}
