package loophost

import (
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
}

// OpenPRs is the repository's open PRs and the files each changes: the GitHub
// read the overlap check needs ([github.Pulls]).
type OpenPRs interface {
	OpenPRs() ([]github.PullRequest, error)
	Files(number int) ([]string, error)
}

var _ loop.Source = (*TicketSource)(nil)

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
	s.prs = prs
	return s
}

// checkout binds the Consumer's checkout and branch prefix to the git seam once,
// so the source's git reads name the ticket and nothing else.
func (s *TicketSource) checkout() gitpkg.Checkout {
	return gitpkg.Open(s.cfg.ProjectPath, s.cfg.BranchPrefix)
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
// item's Branch is the slugged feature branch the Stages will create and push.
func (s *TicketSource) Next() (loop.WorkItem, *lease.Lease, bool, error) {
	sel := pipeline.ResolveNext(s.Resolver(), hostlock.File(config.ClaimLockFile(s.cfg.ProjectPath)), false, s.cfg.Tracker.QueueHint(), s.log)
	if sel.Err != nil {
		return loop.WorkItem{}, nil, false, sel.Err
	}
	if !sel.Proceed {
		return loop.WorkItem{}, nil, false, nil
	}
	key := sel.Lease.Key()
	item := loop.WorkItem{Key: key, Branch: s.checkout().Worktree(ticket.Slug(key)).Branch()}
	return item, sel.Lease, true, nil
}

// Resolver is the tracker selection Next claims through: screened against the
// work in flight when the overlap check is on. `pipeline --next` selects through
// it too, so a single-shot run defers what the loop would.
func (s *TicketSource) Resolver() pipeline.NextResolver {
	if s.prs == nil {
		return s.client
	}
	policy := overlap.Policy{Threshold: s.cfg.OverlapThreshold, EscapeLabel: s.cfg.OverlapEscapeLabel}
	branch := func(key string) string { return s.checkout().Worktree(ticket.Slug(key)).Branch() }
	return pipeline.DeferOverlaps(s.client, s.inFlight, branch, policy, s.log)
}

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

// Comment posts a breadcrumb on the ticket.
func (s *TicketSource) Comment(key, body string) error { return s.client.AddComment(key, body) }

// Claims projects the tracker's agent-claimed In Progress set onto the loop's
// StaleClaim shape (BEH-677), so the daemon stays decoupled from the Tracker port.
func (s *TicketSource) Claims() ([]loop.StaleClaim, error) {
	claims, err := s.client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	out := make([]loop.StaleClaim, 0, len(claims))
	for _, c := range claims {
		out = append(out, loop.StaleClaim{
			Key:         c.Identifier,
			StartedAt:   c.StartedAt,
			HasLinkedPR: c.HasLinkedPR,
		})
	}
	return out, nil
}

// HeldBranches maps each branch the ticket loop holds right now — an agent-claimed
// In Progress ticket's `<branch_prefix>/<slug>`, while its worktree is still
// checked out — to the ticket's Key. The PR loop skips those branches: the lease is
// keyed on the branch, and a ticket mid-run owns its PR's branch (ADR-0019),
// including through an in-stage CI watch. A shipped ticket stays In Progress until
// its PR merges, but the retrospective reaps its worktree once the PR is open; from
// then on the branch is free, so the PR loop can fix CI that goes red after the
// ticket loop moved on (#110).
func (s *TicketSource) HeldBranches() (map[string]string, error) {
	claims, err := s.client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	held := make(map[string]string, len(claims))
	for _, c := range claims {
		wt := s.checkout().Worktree(ticket.Slug(c.Identifier))
		if wt.Exists() {
			held[wt.Branch()] = c.Identifier
		}
	}
	return held, nil
}

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
	return pipeline.Run(ticketDeps(p.loop, cfg, log, runID, ticketArgs(p.loop, item.Key, claim)))
}

// ticketArgs are the stage args for one claimed ticket on loop l. A loop without
// watch_ci leaves the PR's CI to the PR loop, so its review stage returns once the
// PR is open (#110).
func ticketArgs(l config.Loop, key string, claim *lease.Lease) stages.Args {
	return stages.Args{Identifier: key, Lease: claim, LeaveCIToPRLoop: l.LeavesCIToPRLoop()}
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
// branch" the review stage ends on — run over a Host bound to this ticket. A setup
// failure means there is nothing to recover: fall through to the normal release.
func (p *TicketPipeline) Recover(item loop.WorkItem) (stages.Result, bool) {
	cfg, log, runID, err := p.setup(item.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}, false
	}
	rec := ship.Recover(hostio.New(cfg, log, runID, false, ticket.Slug(item.Key)), p.log, item.Key)
	out := stages.Result{}
	if rec.Shipped {
		out = stages.Result{OK: true, Disposition: stages.Shipped}
	}
	return out, rec.Attempted
}
