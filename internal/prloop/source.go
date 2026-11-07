// Package prloop is the PR loop (ADR-0019): the second [loop.Source] and
// [loop.Pipeline] the shared daemon drives. Its Source, `github_prs`, polls the
// open PRs the harness may touch for a merge conflict with main or red CI; its
// Pipeline checks the PR's head branch out, runs the RebaseFix or CIFix session
// over it, checks the result for itself, re-runs the Gates, pushes to the existing
// branch and watches CI to a verdict. Both reuse the review stage's machinery
// (internal/fix) rather than copy it.
//
// By default the loop touches every open PR. A loop with `scope = "opted_in"` (or
// a source.label) touches only PRs that opted in: those on the harness's
// branch_prefix, and those a human labelled for it. It never works a fork (it
// cannot push there).
// Its lease is a label on the PR, and what it tried is recorded in marker comments
// on the PR, so a restarted daemon neither double-works a PR nor forgets its budget.
package prloop

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/fix"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// GitHub is the slice of GitHub the Source reads and writes ([github.Pulls]).
type GitHub interface {
	OpenPRs() ([]github.PullRequest, error)
	MergeState(number int) (ci.MergeVerdict, error)
	Checks(sha string) (github.ChecksVerdict, error)
	Comments(number int) ([]github.PRComment, error)
	Comment(number int, body string) error
	AddLabel(number int, label string) error
	RemoveLabel(number int, label string) error
}

// Kind is what is wrong with a PR.
type Kind string

const (
	// Conflict — the PR conflicts with its base.
	Conflict Kind = "conflict"
	// CIRed — the PR's head commit has a failed check.
	CIRed Kind = "ci"
)

func (k Kind) describe() string {
	if k == Conflict {
		return "merge conflict with main"
	}
	return "red CI"
}

// Options are the Source's knobs.
type Options struct {
	// BranchPrefix is the Consumer's branch_prefix: a PR whose head branch is on it
	// was opened by the harness, which may rewrite its history.
	BranchPrefix string
	// OptInLabel lets the loop work a PR off the branch prefix: a human's branch.
	OptInLabel string
	// AllPRs works every open PR the harness can push to, opted in or not.
	AllPRs bool
	// HeldLabel is the PR lease: the label a run holds on the PR it works.
	HeldLabel string
	// BlockedLabel marks a PR the loop must leave alone, and is what it adds when a
	// PR's attempt budget runs out.
	BlockedLabel string
	// MaxAttempts is the per-PR attempt budget.
	MaxAttempts int
	// BaseBranch is the only base the harness lands on; a PR into another base is
	// not the loop's to fix.
	BaseBranch string
	// Only, when set, is the one failure kind this loop works (its source.when);
	// empty works both.
	Only Kind
	// LogDir is the loop's log dir relative to the checkout, which the hand-off
	// comment points a human at.
	LogDir string
}

// The labels the loop uses when the Consumer's tracker does not already name them.
const (
	defaultHeldLabel    = "in-progress"
	defaultBlockedLabel = "blocked"
)

// OptionsFrom reads the Source's knobs off the Consumer's config and the loop's
// source parameters, which override the PR_LOOP_* env knobs. With GitHub Issues
// as the tracker, the held and blocked labels follow the names its tickets
// already use, so one repository speaks one vocabulary.
func OptionsFrom(cfg config.Config, l config.Loop) Options {
	o := Options{
		BranchPrefix: cfg.BranchPrefix,
		OptInLabel:   cfg.PRLoopLabel,
		HeldLabel:    defaultHeldLabel,
		BlockedLabel: defaultBlockedLabel,
		MaxAttempts:  cfg.PRLoopMaxAttempts,
		BaseBranch:   baseBranch,
		LogDir:       l.LogsDir(),
	}
	if l.Source.Label != "" {
		o.OptInLabel = l.Source.Label
	}
	if l.Source.MaxAttempts > 0 {
		o.MaxAttempts = l.Source.MaxAttempts
	}
	o.AllPRs = l.Source.WorksAllPRs()
	switch conflicts, red := l.Source.Yields(config.WhenConflicting), l.Source.Yields(config.WhenCIFailing); {
	case conflicts && !red:
		o.Only = Conflict
	case red && !conflicts:
		o.Only = CIRed
	}
	if cfg.Tracker.Kind == "github" {
		if l := cfg.Tracker.InProgressLabel; l != "" {
			o.HeldLabel = l
		}
		if l := cfg.Tracker.BlockedLabel; l != "" {
			o.BlockedLabel = l
		}
	}
	return o
}

// baseBranch is the one base the harness lands on (as everywhere else).
const baseBranch = "main"

// Item is a PR work item's [loop.WorkItem.Context]: the PR as it was polled, what
// is wrong with it, and whose branch it is.
type Item struct {
	PR   github.PullRequest
	Kind Kind
	// Harness marks a head branch on the harness's branch_prefix: its history is the
	// harness's to rewrite. Otherwise a human opted the PR in with the label.
	Harness bool
}

// Strategy is how a conflict on this PR is resolved: a harness branch is rebased
// and pushed force-with-lease, a human's branch has main merged into it and is
// pushed normally (ADR-0019).
func (it Item) Strategy() fix.Strategy {
	if it.Harness {
		return fix.Rebase
	}
	return fix.Merge
}

// Attempt is one try the loop recorded on a PR.
type Attempt struct {
	SHA  string
	Kind Kind
	At   time.Time
}

// Candidate is one open PR as the Source judged it: the item it would work, or why
// it skips the PR. The dry-run renders these; Next acts on them.
type Candidate struct {
	Item
	// Skip, when set, says why the PR is not worked.
	Skip string
	// Tried is every attempt recorded on the PR; Attempts counts those against the
	// current budget (since the last hand-off to a human).
	Tried    []Attempt
	Attempts int
	// Exhausted marks a PR past its attempt budget: the loop hands it to a human.
	Exhausted bool
	// Unpushed marks a PR already tried at this head for this kind: that attempt
	// pushed nothing, since a push moves the head. It is retried while budget
	// remains (#167).
	Unpushed bool
	// Worktree is a stale worktree that has the PR's head branch checked out: not
	// the loop's own, with no live run in it, clean, and holding nothing origin's
	// tip lacks. Next removes it before it works anything, so the checkout can
	// succeed (#231); the dry-run only names it.
	Worktree string
}

// Worktrees is the checkout's worktrees as the PR loop reads and reclaims them
// ([git.Checkout]).
type Worktrees interface {
	// CheckedOutBranches maps each branch a worktree has checked out to its path.
	CheckedOutBranches() (map[string]string, error)
	// WorktreeSafeToRemove says why removing the worktree at path would lose work
	// origin's tip does not have, or nil when it would not.
	WorktreeSafeToRemove(path, tip string) error
	// RemoveWorktree removes the worktree at path.
	RemoveWorktree(path string) error
}

// Source is the `github_prs` [loop.Source].
type Source struct {
	gh   GitHub
	opts Options
	// held lists the branches another loop holds right now (branch → the Key it is
	// held under): the ticket loop's In Progress tickets whose run is live. The lease
	// is keyed on the branch, so the PR loop skips a branch whose ticket is still
	// being worked.
	held func() (map[string]string, error)
	// worktrees are the checkout's worktrees, and own is the path of the loop's own
	// worktree for a PR. git refuses to check out a branch another worktree holds
	// (#192). A worktree is not a hold, though (#231): one no live run is in is
	// removed when that loses nothing, and the PR is skipped, with the reason in
	// the stream, only when it would.
	worktrees Worktrees
	own       func(number int) string
	// reported is the last skip narrated per PR, so a PR skipped poll after poll
	// for the same reason is narrated once.
	reported map[int]string
	log      loop.Narrator
	now      func() time.Time
}

var _ loop.Source = (*Source)(nil)

// NewSource builds the Source over GitHub. held reports the branches another loop
// holds; nil means none does.
func NewSource(gh GitHub, opts Options, held func() (map[string]string, error), log loop.Narrator) *Source {
	if held == nil {
		held = func() (map[string]string, error) { return nil, nil }
	}
	return &Source{gh: gh, opts: opts, held: held, reported: map[int]string{}, log: log, now: time.Now}
}

// WatchWorktrees turns on the worktree check (#192, #231): a worktree other than
// own(number) that has a PR's head branch checked out is removed when that loses
// nothing, and otherwise the PR is skipped, with the reason, until it is gone.
func (s *Source) WatchWorktrees(wts Worktrees, own func(number int) string) *Source {
	s.worktrees, s.own = wts, own
	return s
}

// holds is what keeps the loop off a PR's branch for now: the ticket loop's held
// branches (branch → Key), and the branches the checkout's worktrees have checked
// out (branch → worktree path).
type holds struct {
	tickets   map[string]string
	worktrees map[string]string
}

// holds reads both, failing on either: a PR judged without them could be worked
// under the ticket loop, or spend an attempt on a checkout git refuses.
func (s *Source) holds() (holds, error) {
	var h holds
	var err error
	if h.tickets, err = s.held(); err != nil {
		return h, fmt.Errorf("could not read which branches the ticket loop holds: %w", err)
	}
	if s.worktrees != nil {
		if h.worktrees, err = s.worktrees.CheckedOutBranches(); err != nil {
			return h, fmt.Errorf("could not list the checkout's worktrees: %w", err)
		}
	}
	return h, nil
}

// foreignWorktree is the worktree, other than the loop's own for pr, that has pr's
// head branch checked out, or "".
func (s *Source) foreignWorktree(pr github.PullRequest, h holds) string {
	path, ok := h.worktrees[pr.HeadBranch]
	if !ok || (s.own != nil && samePath(path, s.own(pr.Number))) {
		return ""
	}
	return path
}

// samePath reports whether two paths name one directory. git lists a worktree by
// its real path, which a checkout reached through a symlink does not spell the
// same way.
func samePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// CheckAccess proves the GitHub credential can read the PRs, once at startup.
func (s *Source) CheckAccess() error {
	if _, err := s.gh.OpenPRs(); err != nil {
		return fmt.Errorf("could not list the repository's open PRs — check GH_TOKEN and gh: %w", err)
	}
	return nil
}

// Plan judges every open PR without changing anything: it is what Next acts on,
// and what `loop <name> --dry-run` prints. A PR whose state cannot be read is
// skipped with the error, so one bad PR never stalls the poll; a failed list read,
// or not knowing which branches another loop or worktree holds, fails the whole
// poll.
func (s *Source) Plan() ([]Candidate, error) {
	prs, err := s.gh.OpenPRs()
	if err != nil {
		return nil, err
	}
	held, err := s.holds()
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(prs))
	for _, pr := range prs {
		out = append(out, s.judge(pr, held))
	}
	return out, nil
}

// judge decides one PR, cheapest checks first: labels and branch before any read,
// the merge state and checks before the comment history.
func (s *Source) judge(pr github.PullRequest, held holds) Candidate {
	c := Candidate{Item: Item{PR: pr, Harness: strings.HasPrefix(pr.HeadBranch, s.opts.BranchPrefix+"/")}}
	skip := func(format string, a ...any) Candidate { c.Skip = fmt.Sprintf(format, a...); return c }
	worktree := s.foreignWorktree(pr, held)
	switch {
	case pr.Fork:
		return skip("fork — the harness cannot push to it")
	case pr.BaseBranch != s.opts.BaseBranch:
		return skip("base is %s, not %s", pr.BaseBranch, s.opts.BaseBranch)
	case pr.HasLabel(s.opts.BlockedLabel):
		return skip("labelled %s", s.opts.BlockedLabel)
	case !c.Harness && !s.opts.AllPRs && !pr.HasLabel(s.opts.OptInLabel):
		return skip("not opted in (not on %s/ and no %s label)", s.opts.BranchPrefix, s.opts.OptInLabel)
	case pr.HasLabel(s.opts.HeldLabel):
		return skip("held (labelled %s)", s.opts.HeldLabel)
	case held.tickets[pr.HeadBranch] != "":
		return skip("branch held by %s in the ticket loop", held.tickets[pr.HeadBranch])
	case worktree != "":
		// No live run holds the branch, so the worktree is a leftover, not a hold
		// (#231). One that cannot go without losing work is a skip, not an attempt:
		// the checkout cannot succeed, so the budget is not spent on it, and the PR is
		// judged afresh once the worktree is gone (#192).
		if err := s.worktrees.WorktreeSafeToRemove(worktree, pr.HeadSHA); err != nil {
			return skip("branch checked out in worktree %s, which cannot be removed safely: %v", worktree, err)
		}
		c.Worktree = worktree
	}

	merge, err := s.gh.MergeState(pr.Number)
	if err != nil {
		return skip("could not read mergeability: %v", err)
	}
	if merge == ci.MergeConflicting {
		c.Kind = Conflict
	} else {
		checks, err := s.gh.Checks(pr.HeadSHA)
		if err != nil {
			return skip("could not read checks: %v", err)
		}
		switch {
		case checks == github.ChecksRed:
			c.Kind = CIRed
		case checks == github.ChecksPending:
			return skip("checks pending")
		case merge == ci.MergeUnknown:
			return skip("checks green, mergeability not computed yet")
		default:
			return skip("green")
		}
	}
	if s.opts.Only != "" && c.Kind != s.opts.Only {
		return skip("%s — not this loop's (its source.when leaves it out)", c.Kind.describe())
	}

	comments, err := s.gh.Comments(pr.Number)
	if err != nil {
		return skip("could not read its comments: %v", err)
	}
	c.Tried, c.Attempts = attempts(comments)
	c.Exhausted = c.Attempts >= s.opts.MaxAttempts
	// An attempt at this head pushed nothing, so no push will come to start a new
	// item: the PR is retried, spending its budget, until it is handed off (#167).
	for _, a := range c.Tried {
		c.Unpushed = c.Unpushed || (a.SHA == pr.HeadSHA && a.Kind == c.Kind)
	}
	return c
}

// Next works the first PR worth working: it holds the PR's lease, records the
// attempt on the PR, and hands the daemon the item. A PR past its budget is handed
// to a human on the way past.
func (s *Source) Next() (loop.WorkItem, *lease.Lease, bool, error) {
	cands, err := s.Plan()
	if err != nil {
		return loop.WorkItem{}, nil, false, err
	}
	// Reclaim every stale worktree first, whether or not its PR is worked now: a
	// green PR that goes red later must not find its branch still held (#231). A
	// run may have started in the GitHub reads since Plan read the holds, so they
	// are read again first: the loop never removes a worktree a live run is in.
	var live map[string]string
	for i, c := range cands {
		if c.Worktree == "" {
			continue
		}
		if live == nil {
			if live, err = s.held(); err != nil {
				return loop.WorkItem{}, nil, false, fmt.Errorf("could not read which branches the ticket loop holds: %w", err)
			}
			if live == nil {
				live = map[string]string{}
			}
		}
		if by := live[c.PR.HeadBranch]; by != "" {
			cands[i].Skip = fmt.Sprintf("branch held by %s in the ticket loop", by)
			continue
		}
		if err := s.worktrees.RemoveWorktree(c.Worktree); err != nil {
			cands[i].Skip = fmt.Sprintf("branch checked out in worktree %s, whose removal failed: %v", c.Worktree, err)
			continue
		}
		s.log.Event(fmt.Sprintf("loop — removed the stale worktree %s, which held %s of %s: no live run in it, clean, nothing origin lacks (#231)", c.Worktree, c.PR.HeadBranch, Key(c.PR.Number)))
	}
	s.report(cands)
	for _, c := range cands {
		if c.Skip != "" {
			continue
		}
		if c.Exhausted {
			s.handOff(c)
			continue
		}
		key := Key(c.PR.Number)
		q := &labelQueue{gh: s.gh, label: s.opts.HeldLabel}
		claim := lease.Unheld(q, key)
		if err := claim.Hold(); err != nil {
			return loop.WorkItem{}, nil, false, fmt.Errorf("hold %s: %w", key, err)
		}
		if err := s.gh.Comment(c.PR.Number, s.attemptComment(c)); err != nil {
			// Without the record the budget would not count this try: give the lease back.
			_ = claim.Release()
			return loop.WorkItem{}, nil, false, fmt.Errorf("record the attempt on %s: %w", key, err)
		}
		q.released = func() { s.settle(c) }
		retry := ""
		if c.Unpushed {
			retry = "; the previous attempt at this head pushed nothing"
		}
		s.log.Structured(loopstream.Record{Kind: loopstream.KindTicketSelected, Ticket: key, Message: fmt.Sprintf("loop — selected %s (%s at %s on %s, attempt %d of %d%s)", key, c.Kind.describe(), short(c.PR.HeadSHA), c.PR.HeadBranch, c.Attempts+1, s.opts.MaxAttempts, retry)})
		return loop.WorkItem{Key: key, Branch: c.PR.HeadBranch, Context: c.Item}, claim, true, nil
	}
	return loop.WorkItem{}, nil, false, nil
}

// report narrates, into the stream, each PR a worktree keeps the loop off, so the
// skip is not hidden behind "queue empty" (#231). A PR skipped for the same reason
// poll after poll is narrated once; the other skips (green, pending, not opted in)
// are the steady state and stay in the dry-run.
func (s *Source) report(cands []Candidate) {
	seen := map[int]bool{}
	for _, c := range cands {
		if !strings.HasPrefix(c.Skip, "branch checked out in worktree") {
			continue
		}
		seen[c.PR.Number] = true
		if s.reported[c.PR.Number] == c.Skip {
			continue
		}
		s.reported[c.PR.Number] = c.Skip
		key := Key(c.PR.Number)
		s.log.Structured(loopstream.Record{Kind: loopstream.KindItemSkipped, Ticket: key, Message: "loop — skip " + key + ": " + c.Skip})
	}
	for n := range s.reported {
		if !seen[n] {
			delete(s.reported, n)
		}
	}
}

// handOff comments on a PR past its attempt budget with what was tried, and labels
// it blocked so the loop leaves it alone until a human removes the label.
// Best-effort: a failure is narrated and the PR is retried on the next poll.
func (s *Source) handOff(c Candidate) {
	key := Key(c.PR.Number)
	s.log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: key, Message: fmt.Sprintf("loop — %s used its %d attempts; handing it to a human (labelling %s)", key, s.opts.MaxAttempts, s.opts.BlockedLabel)})
	if err := s.gh.Comment(c.PR.Number, s.handOffComment(c)); err != nil {
		s.log.Event("loop … warning: could not comment on " + key + ": " + err.Error())
		return
	}
	if err := s.gh.AddLabel(c.PR.Number, s.opts.BlockedLabel); err != nil {
		s.log.Event("loop … warning: could not label " + key + " " + s.opts.BlockedLabel + ": " + err.Error())
	}
}

// Withdraw takes back the attempt Next recorded on it.PR when git refused the
// checkout on the host's local state before any session ran (#247): a worktree a
// previous run left dirty or locked, or the branch held by another worktree. No
// attempt was made, so none is spent. Retrying would meet the same refusal, so the
// PR is handed to a human with the blocked label, naming the worktree and git's
// error, and the budget it keeps waits for the label to come off. Call it before
// the lease is released, so settling the lease sees the PR blocked. Best-effort:
// a failure is narrated, and an attempt it could not take back stays spent.
func (s *Source) Withdraw(it Item, err error) {
	key := Key(it.PR.Number)
	s.log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: key, Message: fmt.Sprintf("loop — %s: could not check out %s because of local state on the host (%v); not counted as an attempt — handing it to a human (labelling %s)", key, it.PR.HeadBranch, err, s.opts.BlockedLabel)})
	body := fmt.Sprintf("%s\nagent-harness could not check out `%s` because of local state on its host, so no session ran and this does not count as an attempt:\n\n```\n%v\n```\n\nIt is handing the PR to a human. Clean that worktree up, then remove the `%s` label to have the loop work it again.\n",
		voidMarker(it.PR.HeadSHA, it.Kind), it.PR.HeadBranch, err, s.opts.BlockedLabel)
	if err := s.gh.Comment(it.PR.Number, body); err != nil {
		s.log.Event("loop … warning: could not take back the attempt on " + key + ": " + err.Error())
	}
	if err := s.gh.AddLabel(it.PR.Number, s.opts.BlockedLabel); err != nil {
		s.log.Event("loop … warning: could not label " + key + " " + s.opts.BlockedLabel + ": " + err.Error())
	}
}

// settle runs as the lease on c's PR is released, after the run that worked it. A
// run that spent the PR's last attempt and left it failing hands it to a human
// now, not on the next poll: the daemon's breaker may stop it before then, and the
// PR must not sit red with no visible state (#167). Best-effort: anything it cannot
// read leaves the PR to the next poll, which hands off a spent PR the same way.
func (s *Source) settle(c Candidate) {
	if c.Attempts+1 < s.opts.MaxAttempts {
		return
	}
	prs, err := s.gh.OpenPRs()
	if err != nil {
		return
	}
	held, err := s.holds()
	if err != nil {
		return
	}
	for _, pr := range prs {
		if pr.Number != c.PR.Number {
			continue
		}
		// Still failing at the head this run tried: the run pushed nothing.
		if now := s.judge(pr, held); now.Skip == "" && now.Exhausted && now.Unpushed {
			s.handOff(now)
		}
		return
	}
}

// Claims lists the PRs held with the lease label, each timed by its latest attempt
// marker, so the reaper releases a label a dead daemon stranded once it is older
// than the claim TTL. The reaper only ever runs between this daemon's own items, so
// a held PR it sees is never one this daemon is working. One PR loop per checkout.
//
// A held PR with no attempt marker is not this loop's claim: a human labelled it,
// or another loop holds it under the same label. It is left out, since its zero
// start time would be past any TTL and the reaper would strip the label (#196).
func (s *Source) Claims() ([]loop.StaleClaim, error) {
	prs, err := s.gh.OpenPRs()
	if err != nil {
		return nil, err
	}
	var out []loop.StaleClaim
	for _, pr := range prs {
		if !pr.HasLabel(s.opts.HeldLabel) {
			continue
		}
		comments, err := s.gh.Comments(pr.Number)
		if err != nil {
			continue
		}
		tried, _ := attempts(comments)
		if len(tried) == 0 {
			continue
		}
		out = append(out, loop.StaleClaim{Key: Key(pr.Number), StartedAt: tried[len(tried)-1].At})
	}
	return out, nil
}

// HasRemoteBranch is always false: a PR's branch is always on the remote, so it
// says nothing about whether the lease on it is live.
func (s *Source) HasRemoteBranch(string) bool { return false }

// Release removes the lease label from a PR a dead run held.
func (s *Source) Release(key string) error {
	n, err := Number(key)
	if err != nil {
		return err
	}
	return s.gh.RemoveLabel(n, s.opts.HeldLabel)
}

// Block labels the PR blocked, as the attempt budget's hand-off does. The PR
// Pipeline never recovers a branch, so the daemon has no refusal to hand over today.
func (s *Source) Block(key string) error {
	n, err := Number(key)
	if err != nil {
		return err
	}
	return s.gh.AddLabel(n, s.opts.BlockedLabel)
}

// Comment posts nothing. The daemon's breadcrumbs are worded for tickets ("released
// back to Todo") and would mislead on a PR; the PR loop's own record is the attempt
// marker it posts on every try and the hand-off comment past the budget.
func (s *Source) Comment(string, string) error { return nil }

// Releasing narrates the lease label coming off a PR. A PR run pushes to the
// existing PR, so one that ships nothing pushed nothing, rather than "produced
// no PR", and there is no Todo to go back to (#173).
func (s *Source) Releasing(why loop.ReleaseReason, key string) string {
	label := s.opts.HeldLabel
	switch why {
	case loop.ReleaseCapAbort:
		return "spending-cap abort on " + key + "; releasing its " + label + " label"
	case loop.ReleaseStale:
		return "reaping stale claim " + key + " (" + label + " label past the claim TTL); releasing the label"
	default:
		return key + " pushed nothing; releasing its " + label + " label"
	}
}

// labelQueue is the PR lease's [lease.Queue]: holding a PR adds the label, and
// releasing or closing it removes the label. The loop never closes a PR.
type labelQueue struct {
	gh    GitHub
	label string
	// released, when set, runs once the label is off: the Source settles the attempt.
	released func()
}

func (q *labelQueue) MoveToInProgress(key tracker.Key) error { return q.edit(key, q.gh.AddLabel) }
func (q *labelQueue) MoveToCanceled(key tracker.Key) error   { return q.edit(key, q.gh.RemoveLabel) }

func (q *labelQueue) ReleaseToTodo(key tracker.Key) error {
	if err := q.edit(key, q.gh.RemoveLabel); err != nil {
		return err
	}
	if q.released != nil {
		q.released()
	}
	return nil
}

func (q *labelQueue) edit(key tracker.Key, op func(int, string) error) error {
	n, err := Number(key)
	if err != nil {
		return err
	}
	return op(n, q.label)
}

// Key is the work item Key for a PR: `PR-<number>`. It slugs to the worktree and
// log dir `pr-<number>`.
func Key(number int) string { return "PR-" + strconv.Itoa(number) }

// Number is the PR number a Key names.
func Number(key string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(key, "PR-"))
	if err != nil {
		return 0, fmt.Errorf("not a PR key: %q", key)
	}
	return n, nil
}

// --- the record on the PR ------------------------------------------------------

// The markers are HTML comments, invisible in the rendered PR, that make the
// attempt record survive a daemon restart.
const (
	markerPrefix  = "<!-- agent-harness:pr-fix "
	blockedMarker = markerPrefix + "blocked -->"
)

var (
	attemptRE = regexp.MustCompile(`<!-- agent-harness:pr-fix attempt sha=([0-9a-fA-F]+) kind=(\w+) -->`)
	voidRE    = regexp.MustCompile(`<!-- agent-harness:pr-fix void sha=([0-9a-fA-F]+) kind=(\w+) -->`)
)

func attemptMarker(sha string, k Kind) string {
	return fmt.Sprintf("%sattempt sha=%s kind=%s -->", markerPrefix, sha, k)
}

// voidMarker takes back the latest attempt at sha for k: one whose checkout git
// refused before any session ran (#247).
func voidMarker(sha string, k Kind) string {
	return fmt.Sprintf("%svoid sha=%s kind=%s -->", markerPrefix, sha, k)
}

// attempts reads the attempt record off a PR's comments: every attempt not taken
// back, and how many of them came after the last hand-off to a human (which a
// human answers by removing the blocked label, granting a fresh budget).
func attempts(comments []github.PRComment) (tried []Attempt, sinceHandOff int) {
	handedOff := 0 // how many of tried came before the last hand-off
	for _, c := range comments {
		if strings.Contains(c.Body, blockedMarker) {
			handedOff = len(tried)
			continue
		}
		if m := voidRE.FindStringSubmatch(c.Body); m != nil {
			for i := len(tried) - 1; i >= handedOff; i-- {
				if tried[i].SHA == m[1] && tried[i].Kind == Kind(m[2]) {
					tried = append(tried[:i], tried[i+1:]...)
					break
				}
			}
			continue
		}
		if m := attemptRE.FindStringSubmatch(c.Body); m != nil {
			tried = append(tried, Attempt{SHA: m[1], Kind: Kind(m[2]), At: c.CreatedAt})
		}
	}
	return tried, len(tried) - handedOff
}

func (s *Source) attemptComment(c Candidate) string {
	retry := ""
	if c.Unpushed {
		retry = " The previous attempt at this head pushed nothing, so it is trying again."
	}
	return fmt.Sprintf("%s\nagent-harness is fixing the %s at `%s` (attempt %d of %d): %s. It pushes only if the result passes every gate.%s",
		attemptMarker(c.PR.HeadSHA, c.Kind), c.Kind.describe(), short(c.PR.HeadSHA), c.Attempts+1, s.opts.MaxAttempts, planFor(c.Item), retry)
}

func (s *Source) handOffComment(c Candidate) string {
	var b strings.Builder
	b.WriteString(blockedMarker + "\n")
	fmt.Fprintf(&b, "agent-harness has used its attempt budget (%d) on this PR and is handing it to a human. What it tried:\n\n", s.opts.MaxAttempts)
	for _, a := range c.Tried {
		fmt.Fprintf(&b, "- the %s at `%s` (%s)\n", a.Kind.describe(), short(a.SHA), a.At.UTC().Format("2006-01-02 15:04Z"))
	}
	fmt.Fprintf(&b, "\nIt is still failing with a %s at `%s`. The run logs are under `%s/%s/`. Remove the `%s` label to give it another %d attempts.\n",
		c.Kind.describe(), short(c.PR.HeadSHA), s.opts.LogDir, strings.ToLower(Key(c.PR.Number)), s.opts.BlockedLabel, s.opts.MaxAttempts)
	return b.String()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
