package secloop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/depcheck"
	"github.com/danoleary/agent-harness/internal/fix"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/ship"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/verify"
)

// who is the name the security loop's narration speaks in.
const who = "sec-fix"

// sessionLabel keys the SecurityFix session's container and transcript.
const sessionLabel = "securityfix"

// Pipeline is a security-alerts loop's [loop.Pipeline]: [Fix] over one package,
// on a Host bound to the package's branch, with its own per-item log dir and run
// id.
type Pipeline struct {
	Loop config.Loop
}

var _ loop.Pipeline = Pipeline{}

// Run fixes one package. It always gives the lease back when it is done: once the
// PR is open the held label keeps pr-doctor off it only while this run works it,
// and the next poll judges the package afresh.
func (p Pipeline) Run(item loop.WorkItem, claim *lease.Lease) stages.Result {
	defer func() { _ = claim.Release() }()
	it, ok := item.Context.(Item)
	if !ok {
		fmt.Fprintf(os.Stderr, "secloop: %s carries no package context\n", item.Key)
		return stages.Result{}
	}
	cfg, err := stages.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}
	}
	log, err := runlog.New(OptionsFrom(cfg, p.Loop).LogsDir, item.Key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return stages.Result{}
	}
	h := hostio.New(cfg, log, runlog.MakeRunID(time.Now()), false, ticket.Slug(item.Key))
	return Fix(h, cfg, log, it, p.Loop)
}

// Recover finds nothing to recover: every attempt cuts its branch fresh, so a
// branch a dead run left committed is never landed as it stands.
func (Pipeline) Recover(loop.WorkItem) (stages.Result, bool) { return stages.Result{}, false }

// Fix is the security-alerts pipeline over one package, through h, running the
// loop's Steps (ADR-0040):
//
//  1. new_branch: cut the package's branch fresh from origin/main, resetting one a
//     failed attempt left, and run post_create;
//  2. fix_alerts: one SecurityFix session, checked on the host by
//     verify.SecurityFix: a committed diff of dependency files only, and a
//     lockfile that resolves no version inside any alert's vulnerable range;
//  3. gates: every Gate, green;
//  4. open_pr: land the branch as a new PR whose body carries the alerts and the
//     fingerprint marker, and hold it with the held label;
//  5. watch_ci: watch CI to a verdict, and a red check gets pr-doctor's gated CI
//     fix: a CIFix session over the failing logs whose committed fix must pass
//     every gate before the watch may push it. A loop without it stops at the
//     open PR, and pr-doctor fixes red CI once the held label comes off;
//  6. retro: the retrospective mines the run's transcripts and files findings.
//
// Nothing is pushed unless the host check and the gates agree. A landing that
// conflicts pushes nothing, and the started line the lease wrote keeps the
// attempt from being retried until main moves. A landing that collapses means main
// already fixed the package: the ledger records the fingerprint resolved.
func Fix(h hostio.Host, cfg config.Config, log *runlog.Logger, it Item, l config.Loop) stages.Result {
	l.ApplyPrompts(&cfg.Prompts)
	opts := OptionsFrom(cfg, l)
	key := it.Key()
	fixes := fix.Env{Host: h, Cfg: cfg, Log: log, Who: who}
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: key, Stage: who, Message: fmt.Sprintf("run %s — %s on %s at fingerprint %s", h.RunID(), alertNumbers(it.Alerts), it.Branch, it.Fingerprint)})

	if err := h.Preflight(); err != nil {
		return stages.Result{Err: err, Disposition: stages.PreflightAborted}
	}
	if err := h.FetchMain(); err != nil {
		log.Event(who + " … warning: could not fetch origin/main: " + err.Error())
	}
	wt := h.Worktree()
	if err := wt.CreateFresh(); err != nil {
		log.Event(fmt.Sprintf("%s ✗ could not cut %s fresh from origin/main: %v — nothing pushed", who, wt.Branch(), err))
		return stages.Result{}
	}
	r := run{h: h, cfg: cfg, log: log, it: it, opts: opts, fixes: fixes}
	// The branch on origin is the record, and the next attempt cuts it fresh.
	defer fixes.Cleanup()
	if !fixes.Provision() {
		return stages.Result{}
	}

	if out := r.fixAlerts(); out != nil {
		return *out
	}
	if gate := fixes.Gates("gate"); !gate.Green() {
		log.Event(fmt.Sprintf("%s ✗ gate %q is red over the fix (exit %d) — nothing pushed", who, gate.FailedGate, gate.Outcome.ExitCode))
		return stages.Result{}
	}
	out := r.openPR()
	if out.Disposition == stages.Shipped && l.Has(config.StepWatchCI) {
		r.watch()
	}
	if r.sessions && l.Has(config.StepRetro) {
		t := r.ticket()
		t.TeamID = stages.FindingsTeam(h, log, who, key)
		stages.RetrospectiveOver(h, cfg, log, stages.Subject{
			Ticket: t, Slug: ticket.Slug(key), WorktreePath: wt.Path(), Branch: wt.Branch(), LogDir: log.Dir,
		}, false)
	}
	return out
}

// run is one package fix in flight.
type run struct {
	h     hostio.Host
	cfg   config.Config
	log   *runlog.Logger
	it    Item
	opts  Options
	fixes fix.Env
	// sessions marks a run that launched an agent session, which leaves the
	// transcripts a retrospective mines.
	sessions bool
}

// ticket is the package as the sessions' prompts see it: no tracker ticket names
// it, so its Key and a title saying what is to be fixed stand in.
func (r *run) ticket() ticket.Ticket {
	return ticket.Ticket{Identifier: r.it.Key(), Title: title(r.it, r.opts.Severities)}
}

// fixAlerts runs the SecurityFix session and checks its result on the host. It
// returns the run's result when the fix is refused, and nil to go on.
func (r *run) fixAlerts() *stages.Result {
	wt := r.h.Worktree()
	r.sessions = true
	r.log.Event(fmt.Sprintf("%s · launching the SecurityFix session for %s (cap %d min active)", who, alertNumbers(r.it.Alerts), int(r.cfg.ReviewTimeout.Minutes())))
	pctx := prompt.Context{
		Ticket: r.ticket(), Slug: ticket.Slug(r.it.Key()), BranchPrefix: r.cfg.BranchPrefix, Branch: wt.Branch(),
		Tracker: r.cfg.Tracker.Kind, WorktreePath: wt.Path(), Body: r.cfg.Prompts.SecurityFix,
		Alerts: append(append([]github.Alert(nil), r.it.Alerts...), r.it.Unpatched...),
	}
	res := r.h.Agent(hostio.AgentRun{
		Label:    sessionLabel,
		Model:    r.cfg.ReviewModel,
		Prompt:   prompt.For(prompt.SecurityFix, pctx),
		Warnings: prompt.BodyWarnings(pctx),
		Cap:      r.cfg.ReviewTimeout,
		// A dependency bump drives no browser (#248).
		NoMCP:   true,
		WorkDir: wt.Path(),
	})
	eco, _ := depcheck.ForEcosystem(r.it.Ecosystem)
	verdict := verify.SecurityFix(wt, res.Outcome, eco, r.scan(eco))
	switch {
	case verdict.SpendingCapAbort:
		r.log.Event(who + " ↻ " + verdict.Reason)
		return &stages.Result{Disposition: stages.CapAborted}
	case !verdict.OK:
		r.log.Event(who + " ✗ " + verdict.Reason)
		return &stages.Result{}
	}
	r.log.Event(who + " ✓ " + verdict.Reason)
	return nil
}

// scan reads the worktree's lockfile for eco (pnpm-lock.yaml, go.mod) and checks
// it against the patched alerts' vulnerable ranges. An unreadable lockfile, or an
// ecosystem with no reader, is a failed scan, which refuses the fix.
func (r *run) scan(eco depcheck.Ecosystem) depcheck.Result {
	pkg := r.it.Package
	if eco.Lockfile == "" {
		return depcheck.Result{Package: pkg, Err: fmt.Errorf("ecosystem %s has no lockfile reader", r.it.Ecosystem)}
	}
	lock, err := os.ReadFile(filepath.Join(r.h.Worktree().Path(), eco.Lockfile))
	if err != nil {
		return depcheck.Result{Package: pkg, Err: err}
	}
	ranges := make([]string, len(r.it.Alerts))
	for i, a := range r.it.Alerts {
		ranges[i] = a.VulnerableRange
	}
	return eco.Check(lock, pkg, ranges)
}

// openPR lands the gate-green branch as a new PR and holds it with the held label.
// No conflict hook: a lockfile conflict is a fresh attempt once main moves, not a
// resolution session.
func (r *run) openPR() stages.Result {
	landing := ship.LandAs(r.h, r.log, ship.Opening{
		Title: title(r.it, r.opts.Severities),
		Body:  func(subjects []string, _ string) string { return body(r.it, subjects) },
	}, nil)
	switch landing.Verdict {
	case ship.Landed:
		r.log.Event(who + " ✓ opened " + landing.URL)
		if err := r.h.LabelPR(landing.URL, r.opts.HeldLabel); err != nil {
			r.log.Event(fmt.Sprintf("%s … warning: could not label %s %s: %v", who, landing.URL, r.opts.HeldLabel, err))
		}
		return stages.Result{OK: true, Disposition: stages.Shipped}
	case ship.Collapsed:
		r.log.Event(fmt.Sprintf("%s ⊘ origin/main already fixes %s — nothing to ship; recording fingerprint %s resolved", who, r.it.Package, r.it.Fingerprint))
		e := Entry{Key: r.it.Key(), FP: r.it.Fingerprint, BaseSHA: r.mainSHA(), At: time.Now(), Outcome: OutcomeResolved}
		if err := appendLedger(filepath.Join(r.opts.LogsDir, LedgerFile), e); err != nil {
			r.log.Event(who + " … warning: could not record the fingerprint resolved: " + err.Error())
		}
		return stages.Result{Disposition: stages.RecommendClose}
	case ship.Conflict:
		r.log.Event(fmt.Sprintf("%s ✗ %s no longer replays onto origin/main (a sibling changed the lockfile) — nothing pushed; retried once main moves", who, r.it.Branch))
	default:
		r.log.Event(fmt.Sprintf("%s ✗ could not land %s (verdict %d): %v", who, r.it.Branch, landing.Verdict, landing.Err))
	}
	return stages.Result{}
}

// mainSHA is the origin/main the collapsed replay landed on, which the branch's
// base now is.
func (r *run) mainSHA() string {
	sha, _ := r.h.Worktree().BaseSHA()
	return sha
}

// watch watches CI to a verdict. A red check gets the gated CI fix pr-doctor
// runs, and the watch may rebase the branch: it is the harness's own. The PR is
// open either way, so the verdict is only narrated.
func (r *run) watch() {
	gated := r.fixes.GatedCI(fix.Target{Slug: ticket.Slug(r.it.Key()), Ticket: r.ticket(), Branch: r.it.Branch})
	r.log.Event("watching CI for " + r.it.Branch + " …")
	res := r.h.WatchCI(hostio.CIWatch{Fix: gated.Fix})
	r.sessions = r.sessions || gated.Attempts > 0
	if res.OK {
		r.log.Event(who + " ✓ " + res.Reason)
		return
	}
	r.log.Event(who + " ✗ CI did not go green: " + res.Reason)
}

// title is the PR's title: `fix(deps): patch <pkg> (<n> <severities> alerts)`.
func title(it Item, severities []string) string {
	sev := strings.Join(severities, "/")
	if sev == "" {
		sev = "Dependabot"
	}
	noun := "alerts"
	if len(it.Alerts) == 1 {
		noun = "alert"
	}
	return fmt.Sprintf("fix(deps): patch %s (%d %s %s)", it.Package, len(it.Alerts), sev, noun)
}

// body is the PR's description: the alerts it fixes with links, the ones no patch
// exists for, the commits, and the fingerprint marker the Source reads back.
func body(it Item, subjects []string) string {
	var b strings.Builder
	eco, _ := depcheck.ForEcosystem(it.Ecosystem)
	fmt.Fprintf(&b, "Moves `%s` (%s) to a patched version, clearing the Dependabot alerts below. The host checked that `%s` resolves no version inside any of their vulnerable ranges, and that the diff touches dependency files only.\n\n", it.Package, it.Ecosystem, eco.Lockfile)
	b.WriteString("| Alert | Severity | Advisory | Vulnerable range | First patched | Manifest |\n|---|---|---|---|---|---|\n")
	for _, a := range it.Alerts {
		advisory := a.GHSA
		if a.CVE != "" {
			advisory += " / " + a.CVE
		}
		cells := []string{alertLink(a), a.Severity, advisory, a.VulnerableRange, a.Patched, a.ManifestPath}
		for i, c := range cells {
			cells[i] = strings.ReplaceAll(strings.ReplaceAll(c, "\n", " "), "|", `\|`)
		}
		fmt.Fprintf(&b, "| %s |\n", strings.Join(cells, " | "))
	}
	if len(it.Unpatched) > 0 {
		b.WriteString("\nLeft open — no patched version exists, so no dependency change can fix them:\n\n")
		for _, a := range it.Unpatched {
			fmt.Fprintf(&b, "- %s %s (%s, %s)\n", alertLink(a), a.Summary, a.Severity, a.VulnerableRange)
		}
	}
	if len(subjects) > 0 {
		b.WriteString("\n## Commits\n\n")
		for _, s := range subjects {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	b.WriteString("\n" + Marker(it.Fingerprint) + "\n")
	return b.String()
}

func alertLink(a github.Alert) string {
	if a.URL == "" {
		return fmt.Sprintf("#%d", a.Number)
	}
	return fmt.Sprintf("[#%d](%s)", a.Number, a.URL)
}
