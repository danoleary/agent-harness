// Package secloop is the security-alerts loop (ADR-0040): the third [loop.Source]
// and [loop.Pipeline] the shared daemon drives. Its Source, `github_security_alerts`, reads the open
// Dependabot alerts, groups them into one Work item per vulnerable package, and
// judges each against the PRs on the package's branch and an attempt ledger in the
// loop's logs, so a restarted daemon neither re-opens a fix a human closed nor
// forgets its budget. Its Pipeline, [Fix], cuts each package's branch fresh, runs
// one SecurityFix session, checks the lockfile on the host, and lands the fix as a
// new PR.
//
// The lease is keyed on the branch (ADR-0019, decision 2). Before a PR exists the
// run holds it alone, and holding it writes a started line to the ledger; once
// ship opens the PR, the PR carries the held label, as a pr-doctor lease does.
package secloop

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// GitHub is the slice of GitHub the Source reads and writes ([github.Pulls]).
type GitHub interface {
	DependabotAlerts(severity []string) ([]github.Alert, error)
	PRsForBranch(branch string) ([]github.BranchPR, error)
	OpenPRs() ([]github.PullRequest, error)
	RemoveLabel(number int, label string) error
}

// Options are the Source's knobs.
type Options struct {
	// BranchPrefix is the Consumer's branch_prefix, which each package's branch is
	// cut on.
	BranchPrefix string
	// Severities are the alert severities the loop works; empty works every one.
	Severities []string
	// HeldLabel is the lease on a package's PR once ship has opened it.
	HeldLabel string
	// BlockedLabel on any PR from a package's branch keeps the loop off it.
	BlockedLabel string
	// MaxAttempts is the attempt budget per fingerprint.
	MaxAttempts int
	// Grace is how long after a fix merges the loop waits for Dependabot to close
	// its alerts before it reports them still open.
	Grace time.Duration
	// LogsDir is the loop's logs dir, which holds the ledger.
	LogsDir string
}

// Group is every open alert on one package in one ecosystem, across manifests:
// Alerts have a patched version, Unpatched have none.
type Group struct {
	Ecosystem string
	Package   string
	Alerts    []github.Alert
	Unpatched []github.Alert
}

// Key is the group's Work item Key, `SEC-<ecosystem>-<package>`.
func (g Group) Key() string { return "SEC-" + g.Ecosystem + "-" + g.Package }

// Item is a package Work item's [loop.WorkItem.Context]: the group, the branch its
// fix is cut on, and the fingerprint of the alerts it fixes.
type Item struct {
	Group
	Branch      string
	Fingerprint string
}

// Candidate is one package as the Source judged it: the item it would work, or why
// it skips it. The dry-run renders these; Next acts on them.
type Candidate struct {
	Item
	// Skip, when set, says why the package is not worked.
	Skip string
	// Attempts counts the ledger's attempts at this fingerprint.
	Attempts int
	// Exhausted marks a fingerprint at its attempt budget: the loop hands it to a
	// human.
	Exhausted bool
	// PR is the PR on the branch a skip is about, if one is.
	PR *github.BranchPR
}

// Source is the `github_security_alerts` [loop.Source].
type Source struct {
	gh   GitHub
	opts Options
	// base reads origin/main's SHA: an attempt that pushed nothing is not retried
	// until it moves.
	base func() (string, error)
	log  loop.Narrator
	now  func() time.Time
}

var _ loop.Source = (*Source)(nil)

// NewSource builds the Source over GitHub. base reads origin/main's SHA.
func NewSource(gh GitHub, opts Options, base func() (string, error), log loop.Narrator) *Source {
	return &Source{gh: gh, opts: opts, base: base, log: log, now: time.Now}
}

func (s *Source) ledgerPath() string { return filepath.Join(s.opts.LogsDir, LedgerFile) }

// Branch is the branch a Key's fix is cut on, `<branch_prefix>/<slug>`.
func (s *Source) Branch(key string) string { return s.opts.BranchPrefix + "/" + ticket.Slug(key) }

// CheckAccess proves the GitHub credential can read the alerts, once at startup.
func (s *Source) CheckAccess() error {
	if _, err := s.gh.DependabotAlerts(s.opts.Severities); err != nil {
		return fmt.Errorf("could not list the repository's Dependabot alerts: %w", err)
	}
	return nil
}

// Plan judges every vulnerable package without changing anything: it is what Next
// acts on, and what the dry-run prints. A failed alert, ledger or base read fails
// the whole poll; a branch whose PRs cannot be read is skipped with the error.
func (s *Source) Plan() ([]Candidate, error) {
	cands, _, err := s.plan()
	return cands, err
}

// plan is Plan, returning the origin/main SHA it judged against too.
func (s *Source) plan() ([]Candidate, string, error) {
	alerts, err := s.gh.DependabotAlerts(s.opts.Severities)
	if err != nil {
		return nil, "", err
	}
	entries, err := readLedger(s.ledgerPath())
	if err != nil {
		return nil, "", fmt.Errorf("could not read the attempt ledger: %w", err)
	}
	base, err := s.base()
	if err != nil {
		return nil, "", fmt.Errorf("could not read origin/main: %w", err)
	}
	groups := groupAlerts(alerts)
	out := make([]Candidate, 0, len(groups))
	for _, g := range groups {
		out = append(out, s.judge(g, entries, base))
	}
	return out, base, nil
}

// groupAlerts makes one Group per (ecosystem, package), ordered by Key, each
// group's alerts in number order.
func groupAlerts(alerts []github.Alert) []Group {
	byKey := map[string]*Group{}
	for _, a := range alerts {
		g := Group{Ecosystem: a.Ecosystem, Package: a.Package}
		p := byKey[g.Key()]
		if p == nil {
			p = &g
			byKey[g.Key()] = p
		}
		if a.Patched == "" {
			p.Unpatched = append(p.Unpatched, a)
		} else {
			p.Alerts = append(p.Alerts, a)
		}
	}
	out := make([]Group, 0, len(byKey))
	for _, g := range byKey {
		sortAlerts(g.Alerts)
		sortAlerts(g.Unpatched)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func sortAlerts(as []github.Alert) {
	sort.Slice(as, func(i, j int) bool { return as[i].Number < as[j].Number })
}

// Fingerprint is the first 10 hex digits of the SHA-1 over the package, its
// alerts' numbers in order, and each one's first patched version: a new alert, or
// a moved patch, changes it and starts a fresh budget (ADR-0040, decision 7).
func Fingerprint(g Group) string {
	as := append([]github.Alert(nil), g.Alerts...)
	sortAlerts(as)
	h := sha1.New()
	h.Write([]byte(g.Package))
	for _, a := range as {
		fmt.Fprintf(h, "\n%d %s", a.Number, a.Patched)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}

// Marker is the hidden line a fix's PR body carries, naming the fingerprint it
// fixes, so a later poll knows which alert set a closed or merged PR was for.
func Marker(fp string) string { return "<!-- agent-harness:sec-fix fp=" + fp + " -->" }

// lockfiles are the manifests, per ecosystem, the host check has a lockfile reader
// for (internal/depcheck). An alert on any other is skipped: its fix could not be
// checked (ADR-0040, decision 4).
var lockfiles = map[string][]string{
	"npm": {"pnpm-lock.yaml", "package.json"},
}

// unsupported says why no fix of g could be checked on the host, or "".
func unsupported(g Group) string {
	names, ok := lockfiles[g.Ecosystem]
	if !ok {
		return fmt.Sprintf("ecosystem %s is not supported (no lockfile reader)", g.Ecosystem)
	}
	for _, a := range append(append([]github.Alert(nil), g.Alerts...), g.Unpatched...) {
		base := path.Base(a.ManifestPath)
		supported := false
		for _, n := range names {
			supported = supported || base == n
		}
		if !supported {
			return fmt.Sprintf("alert #%d is in %s, which has no lockfile reader", a.Number, a.ManifestPath)
		}
	}
	return ""
}

// judge decides one package, cheapest checks first: the alerts themselves, then
// the PRs on its branch, then the ledger.
func (s *Source) judge(g Group, entries []Entry, base string) Candidate {
	c := Candidate{Item: Item{Group: g, Branch: s.Branch(g.Key()), Fingerprint: Fingerprint(g)}}
	skip := func(format string, a ...any) Candidate { c.Skip = fmt.Sprintf(format, a...); return c }
	if why := unsupported(g); why != "" {
		return skip("%s", why)
	}
	if len(g.Alerts) == 0 {
		return skip("no patched version for %s", alertNumbers(g.Unpatched))
	}

	prs, err := s.gh.PRsForBranch(c.Branch)
	if err != nil {
		return skip("could not read the PRs on %s: %v", c.Branch, err)
	}
	marked := Marker(c.Fingerprint)
	for _, check := range []func(github.BranchPR) string{
		func(pr github.BranchPR) string {
			if pr.State == "open" {
				return fmt.Sprintf("open PR #%d on %s", pr.Number, c.Branch)
			}
			return ""
		},
		func(pr github.BranchPR) string {
			if hasLabel(pr.Labels, s.opts.BlockedLabel) {
				return fmt.Sprintf("PR #%d is labelled %s", pr.Number, s.opts.BlockedLabel)
			}
			return ""
		},
		func(pr github.BranchPR) string {
			if !pr.Merged() && strings.Contains(pr.Body, marked) {
				return fmt.Sprintf("a human closed PR #%d, which fixed these alerts", pr.Number)
			}
			return ""
		},
		func(pr github.BranchPR) string {
			if !pr.Merged() || !strings.Contains(pr.Body, marked) {
				return ""
			}
			if s.now().Sub(pr.MergedAt) < s.opts.Grace {
				return fmt.Sprintf("PR #%d merged this fix %s ago; waiting for Dependabot to close the alerts", pr.Number, s.now().Sub(pr.MergedAt).Round(time.Minute))
			}
			return fmt.Sprintf("PR #%d merged this fix, but %s are still open past the %s grace period; a human must look", pr.Number, alertNumbers(g.Alerts), s.opts.Grace)
		},
	} {
		for i := range prs {
			if why := check(prs[i]); why != "" {
				c.PR = &prs[i]
				return skip("%s", why)
			}
		}
	}

	t := tallyOf(entries, g.Key(), c.Fingerprint)
	c.Attempts = t.attempts
	switch {
	case t.handedOff:
		return skip("handed to a human after %d attempts at fingerprint %s", t.attempts, c.Fingerprint)
	case t.resolved && s.now().Sub(t.resolvedAt) < s.opts.Grace:
		return skip("origin/main already fixed fingerprint %s %s ago; waiting for Dependabot to close the alerts", c.Fingerprint, s.now().Sub(t.resolvedAt).Round(time.Minute))
	case t.resolved:
		return skip("origin/main already fixed fingerprint %s, but %s are still open past the %s grace period; a human must look", c.Fingerprint, alertNumbers(g.Alerts), s.opts.Grace)
	case t.attempts >= s.opts.MaxAttempts:
		c.Exhausted = true
	case t.bases[base]:
		return skip("an attempt at origin/main %s pushed nothing; waiting for main to move", short(base))
	}
	return c
}

func hasLabel(labels []string, label string) bool {
	for _, l := range labels {
		if l == label {
			return true
		}
	}
	return false
}

func alertNumbers(as []github.Alert) string {
	ns := make([]string, len(as))
	for i, a := range as {
		ns[i] = fmt.Sprintf("#%d", a.Number)
	}
	if len(ns) == 1 {
		return "alert " + ns[0]
	}
	return "alerts " + strings.Join(ns, ", ")
}

// Next works the first package worth working: it holds the lease, which records
// the attempt in the ledger, and hands the daemon the item. A fingerprint past its
// budget is handed to a human on the way past.
func (s *Source) Next() (loop.WorkItem, *lease.Lease, bool, error) {
	cands, base, err := s.plan()
	if err != nil {
		return loop.WorkItem{}, nil, false, err
	}
	for _, c := range cands {
		if c.Skip != "" {
			continue
		}
		if c.Exhausted {
			s.handOff(c, base)
			continue
		}
		key := c.Key()
		claim := lease.Unheld(&branchQueue{s: s, item: c.Item, base: base}, key)
		if err := claim.Hold(); err != nil {
			return loop.WorkItem{}, nil, false, fmt.Errorf("hold %s: %w", key, err)
		}
		s.log.Structured(loopstream.Record{Kind: loopstream.KindTicketSelected, Ticket: key, Message: fmt.Sprintf("loop — selected %s (%s at fingerprint %s on %s, attempt %d of %d)", key, alertNumbers(c.Alerts), c.Fingerprint, c.Branch, c.Attempts+1, s.opts.MaxAttempts)})
		return loop.WorkItem{Key: key, Branch: c.Branch, Context: c.Item}, claim, true, nil
	}
	return loop.WorkItem{}, nil, false, nil
}

// handOff records that c's fingerprint used its budget, so the loop leaves the
// package to a human until its alerts change. Best-effort: a failed write is
// narrated and the package is handed off again on the next poll.
func (s *Source) handOff(c Candidate, base string) {
	key := c.Key()
	s.log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: key, Message: fmt.Sprintf("loop — %s used its %d attempts at fingerprint %s; handing it to a human", key, s.opts.MaxAttempts, c.Fingerprint)})
	if err := appendLedger(s.ledgerPath(), Entry{Key: key, FP: c.Fingerprint, BaseSHA: base, At: s.now(), Outcome: OutcomeHandedOff}); err != nil {
		s.log.Event("loop … warning: could not record the hand-off of " + key + ": " + err.Error())
	}
}

// Claims lists the PRs on sec- branches held with the lease label, each timed by
// its item's latest started line, so the reaper releases a label a dead daemon
// stranded. A labelled PR the ledger never started is not this loop's claim: a
// human labelled it, and its zero start time would be past any TTL (#196).
func (s *Source) Claims() ([]loop.StaleClaim, error) {
	prs, err := s.gh.OpenPRs()
	if err != nil {
		return nil, err
	}
	entries, err := readLedger(s.ledgerPath())
	if err != nil {
		return nil, err
	}
	// The branch slug is lossy, so a PR's Key is found from the ledger's Keys.
	started := map[string]loop.StaleClaim{}
	for _, e := range entries {
		if e.Outcome == OutcomeStarted {
			started[s.Branch(e.Key)] = loop.StaleClaim{Key: e.Key, StartedAt: e.At}
		}
	}
	var out []loop.StaleClaim
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadBranch, s.opts.BranchPrefix+"/sec-") || !pr.HasLabel(s.opts.HeldLabel) {
			continue
		}
		if claim, ok := started[pr.HeadBranch]; ok {
			out = append(out, claim)
		}
	}
	return out, nil
}

// HasRemoteBranch is always false: a pushed branch has a PR, whose label is the
// lease, so the branch says nothing more about whether the lease is live.
func (s *Source) HasRemoteBranch(string) bool { return false }

// Release removes the lease label from the open PR on key's branch, if it has one.
func (s *Source) Release(key string) error {
	prs, err := s.gh.OpenPRs()
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if pr.HeadBranch == s.Branch(key) && pr.HasLabel(s.opts.HeldLabel) {
			return s.gh.RemoveLabel(pr.Number, s.opts.HeldLabel)
		}
	}
	return nil
}

// Comment posts nothing. The daemon's breadcrumbs are worded for tickets ("released
// back to Todo"), and before ship there is nowhere to post them; the loop's record
// is the ledger.
func (s *Source) Comment(string, string) error { return nil }

// Block labels nothing. Only the committed-fix recovery hands an item over
// blocked (#230), and this loop's Pipeline recovers nothing: every attempt cuts
// its branch fresh.
func (s *Source) Block(string) error { return nil }

// Releasing narrates the lease coming off a package: there is no Todo to go back
// to, and a run that ships nothing leaves only its started line.
func (s *Source) Releasing(why loop.ReleaseReason, key string) string {
	switch why {
	case loop.ReleaseCapAbort:
		return "spending-cap abort on " + key + "; releasing its branch"
	case loop.ReleaseStale:
		return "reaping stale claim " + key + " (" + s.opts.HeldLabel + " label past the claim TTL); releasing the label"
	default:
		return key + " opened no PR; releasing its branch"
	}
}

// branchQueue is the package lease's [lease.Queue], keyed on the branch: holding
// it records the attempt in the ledger, and releasing or closing it takes the held
// label off the branch's PR, if ship opened one.
type branchQueue struct {
	s    *Source
	item Item
	base string
}

func (q *branchQueue) MoveToInProgress(key tracker.Key) error {
	return appendLedger(q.s.ledgerPath(), Entry{Key: key, FP: q.item.Fingerprint, BaseSHA: q.base, At: q.s.now(), Outcome: OutcomeStarted})
}

func (q *branchQueue) ReleaseToTodo(key tracker.Key) error  { return q.s.Release(key) }
func (q *branchQueue) MoveToCanceled(key tracker.Key) error { return q.s.Release(key) }

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
