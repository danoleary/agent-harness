package loophost

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/hostlock"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
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
}

var _ loop.Source = (*TicketSource)(nil)

// NewTicketSource builds the ticket Source over the Consumer's Tracker.
func NewTicketSource(cfg config.Config, log loop.Narrator, client tracker.Tracker) *TicketSource {
	return &TicketSource{cfg: cfg, log: log, client: client}
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
	sel := pipeline.ResolveNext(s.client, hostlock.File(config.ClaimLockFile(s.cfg.ProjectPath)), false, s.cfg.Tracker.QueueHint(), s.log)
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
// In Progress ticket's `<branch_prefix>/<slug>` — to the ticket's Key. The PR loop
// skips those branches: the lease is keyed on the branch, and a ticket mid-run owns
// its PR's branch (ADR-0019).
func (s *TicketSource) HeldBranches() (map[string]string, error) {
	claims, err := s.client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	held := make(map[string]string, len(claims))
	for _, c := range claims {
		held[s.checkout().Worktree(ticket.Slug(c.Identifier)).Branch()] = c.Identifier
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
	args := stages.Args{Identifier: item.Key, Lease: claim}
	// One Host for the whole slice: the three stages share the run id it stamps into
	// every container name and transcript, and the tracker client it resolves once.
	host := hostio.New(cfg, log, runID, args.Verbose)
	return pipeline.Run(ticketDeps(p.loop, host, cfg, log, args))
}

// ticketDeps binds the loop's Stages to one ticket. A loop without the retro step
// runs no retrospective.
func ticketDeps(l config.Loop, host hostio.Host, cfg config.Config, log *runlog.Logger, args stages.Args) pipeline.Deps {
	d := pipeline.Deps{
		FetchMain:      host.FetchMain,
		Implementation: func() stages.Result { return stages.Implementation(host, cfg, log, args) },
		Review:         func() stages.Result { return stages.Review(host, cfg, log, args) },
		Log:            log,
	}
	if l.Has(config.StepRetro) {
		d.Retrospective = func() stages.Result { return stages.Retrospective(host, cfg, log, args) }
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
	rec := ship.Recover(hostio.New(cfg, log, runID, false), p.log, item.Key)
	out := stages.Result{}
	if rec.Shipped {
		out = stages.Result{OK: true, Disposition: stages.Shipped}
	}
	return out, rec.Attempted
}
