package hostio

import (
	"fmt"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/findings"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// Fake is the scripted [Host] the stage tests drive. It is the second adapter
// that makes [Host] a real seam rather than a hypothetical one: the three Stage
// bodies hold the harness's entire policy, and before it existed the only way to
// reach a single line of them was a live Docker daemon and a live tracker.
//
// [NewFake] returns a *healthy* host — preflight green, sessions exiting 0 with a
// review verdict emitted, fetches and pushes succeeding, gh answering — so a test
// overrides only the one signal it is about and every other field stays out of its
// way.
//
// Git is not scripted. The Fake is built over a real [gitpkg.Checkout] — a temp
// repo with a local bare origin, in a test (internal/gittest) — and its Worktree is
// that checkout's real worktree for the ticket. A worktree that exists, is clean,
// is ahead, is disjoint or has collapsed is one git made so, and a session that
// "commits" does so with git from inside AgentFn. Boolean flags could state
// impossible combinations (no worktree, yet clean) and could not change mid-run
// without a per-test subclass; real git can do neither (#28, ADR-0025).
//
// Fake is deliberately in the non-test build: internal/stages, internal/pipeline
// and (later) internal/loop all need it, and a _test.go fake is importable by
// none of them.
type Fake struct {
	// --- sandbox ---

	// PreflightErr is what Preflight returns (nil = a launchable host).
	PreflightErr error
	// AgentOutcome / ShellOutcome are returned by every Agent / Shell call unless
	// the matching Fn below is set.
	AgentOutcome session.Outcome
	ShellOutcome session.Outcome
	// AgentFn / ShellFn script per-call outcomes — the 1-based call index lets a
	// test make attempt 2 differ from attempt 1 (the retry paths).
	//
	// Neither honours AgentRun.Retry: a fake that re-ran a scripted outcome would
	// be testing the Runner's retry loop through two layers of pretend. That loop
	// is tested directly, against the Runner, in runner_test.go.
	AgentFn func(call int, run AgentRun) session.Outcome
	ShellFn func(call int, run ShellRun) session.Outcome

	// --- repo ---

	co   gitpkg.Checkout
	slug string
	// Branch, when set, is the branch the worktree carries in place of
	// `<branch_prefix>/<slug>` — a PR's head branch, as for [NewOnBranch].
	Branch string
	// Resolved is what ResolvedAdvisory returns: the advisory scans the Consumer's
	// source roots, not git.
	Resolved string

	// --- remote ---

	// FetchErr, PushErr and ForcePushErr fail the matching remote call. Unset, the
	// call really runs against the checkout's origin — a local bare repo in a test —
	// so a push reaches origin and BranchPushed reads true after it.
	FetchErr     error
	PushErr      error
	ForcePushErr error
	PRURL        string
	CreatePRErr  error
	PRThere      bool
	OpenPRThere  bool
	ChecksOK     bool
	ChecksDetail string
	CI           ci.Outcome
	// CIFn, when set, is called with the watch so a test can drive the Fix
	// callback (the sandboxed auto-fix session) and return a scripted outcome.
	CIFn func(CIWatch) ci.Outcome

	// --- tracker ---

	Trk        *FakeTracker
	TrackerErr error
	Prior      []filing.PriorFinding

	// --- recorded ---

	Agents  []AgentRun
	Shells  []ShellRun
	Watches []CIWatch
	Filed   []string
	Routed  []string
	Calls   []string
}

var _ Host = (*Fake)(nil)

// NewFake returns a Fake scripted as a healthy host on the happy path, bound to the
// ticket slug over co.
func NewFake(co gitpkg.Checkout, slug string) *Fake {
	return &Fake{
		AgentOutcome: session.Outcome{ReviewVerdictEmitted: true},
		co:           co,
		slug:         slug,
		PRURL:        "https://github.com/acme/widgets/pull/1",
		PRThere:      true,
		OpenPRThere:  true,
		ChecksOK:     true,
		CI:           ci.Outcome{OK: true, Reason: "all checks green"},
		Trk:          NewFakeTracker(),
	}
}

// RunID is the fake run id every scripted name is stamped with.
func (f *Fake) RunID() string { return "fakerun" }

func (f *Fake) record(format string, a ...any) { f.Calls = append(f.Calls, fmt.Sprintf(format, a...)) }

// --- sandbox ---

func (f *Fake) Preflight() error { f.record("preflight"); return f.PreflightErr }

func (f *Fake) Agent(a AgentRun) Result {
	f.Agents = append(f.Agents, a)
	f.record("agent %s", a.Label)
	out := f.AgentOutcome
	if f.AgentFn != nil {
		out = f.AgentFn(len(f.Agents), a)
	}
	return Result{Outcome: out, Container: "fake-" + a.Label, Transcript: a.Label + ".jsonl", Attempts: 1}
}

func (f *Fake) Shell(s ShellRun) Result {
	f.Shells = append(f.Shells, s)
	f.record("shell %s", s.Label)
	out := f.ShellOutcome
	if f.ShellFn != nil {
		out = f.ShellFn(len(f.Shells), s)
	}
	return Result{Outcome: out, Container: "fake-" + s.Label, Transcript: s.Label + ".log", Attempts: 1}
}

func (f *Fake) AgentPreview(a AgentRun) []string {
	return []string{"run", "--name", "fake-" + a.Label, "claude", "-p", a.Prompt}
}

func (f *Fake) ShellPreview(s ShellRun) []string {
	return []string{"run", "--name", "fake-" + s.Label, "bash", "-c", s.Command}
}

// --- repo ---

// Worktree is the ticket's real worktree in the Fake's checkout.
func (f *Fake) Worktree() gitpkg.Worktree {
	if f.Branch != "" {
		return f.co.WorktreeOn(f.slug, f.Branch)
	}
	return f.co.Worktree(f.slug)
}

func (f *Fake) TicketAlreadyOnMain(key string) bool    { return f.co.TicketAlreadyOnMain(key) }
func (f *Fake) ResolvedAdvisory(string, string) string { return f.Resolved }

// --- remote ---

func (f *Fake) FetchMain() error {
	f.record("fetch-main")
	if f.FetchErr != nil {
		return f.FetchErr
	}
	return f.co.FetchMain()
}

func (f *Fake) Push() error {
	f.record("push")
	if f.PushErr != nil {
		return f.PushErr
	}
	return f.Worktree().Push()
}

func (f *Fake) PushForceWithLease() error {
	f.record("push-force")
	if f.ForcePushErr != nil {
		return f.ForcePushErr
	}
	return f.Worktree().PushForceWithLease()
}

func (f *Fake) CreatePR(title, body string) (string, error) {
	f.record("create-pr %s", title)
	if f.CreatePRErr != nil {
		return "", f.CreatePRErr
	}
	return f.PRURL, nil
}

func (f *Fake) PRExists() bool                 { f.record("pr-exists"); return f.PRThere }
func (f *Fake) OpenPRExists() bool             { f.record("open-pr-exists"); return f.OpenPRThere }
func (f *Fake) ChecksReadable() (bool, string) { return f.ChecksOK, f.ChecksDetail }

func (f *Fake) WatchCI(w CIWatch) ci.Outcome {
	f.Watches = append(f.Watches, w)
	f.record("watch-ci")
	if f.CIFn != nil {
		return f.CIFn(w)
	}
	return f.CI
}

// --- tracker + findings ---

func (f *Fake) Tracker() (tracker.Tracker, error) {
	if f.TrackerErr != nil {
		return nil, f.TrackerErr
	}
	return f.Trk, nil
}

func (f *Fake) AlreadyFiled(string, string) []filing.PriorFinding { return f.Prior }

func (f *Fake) FileFindings(findingsDir, teamID, key string) {
	f.Filed = append(f.Filed, key)
	f.record("file-findings %s", key)
}

func (f *Fake) RouteFindings(findingsDir, teamID, key string) {
	f.Routed = append(f.Routed, key)
	f.record("route-findings %s", key)
}

// FakeTracker is the scripted tracker.Tracker behind [Fake]. Every mutation is
// recorded so a test can assert the claim/release choreography (ADR-0003) without
// a live tracker.
type FakeTracker struct {
	Ticket    ticket.Ticket
	FetchErr  error
	ClaimErr  error
	ReleErr   error
	CancelErr error
	CommentEr error
	ReadyErr  error

	Fetched  []string
	Claimed  []string
	Released []string
	Canceled []string
	Comments []string
}

var _ tracker.Tracker = (*FakeTracker)(nil)

// NewFakeTracker returns a tracker that resolves one ordinary ticket.
func NewFakeTracker() *FakeTracker {
	return &FakeTracker{Ticket: ticket.Ticket{Identifier: "PROJ-1", Title: "a ticket", TeamID: "team"}}
}

func (t *FakeTracker) FetchTicket(key tracker.Key) (ticket.Ticket, error) {
	t.Fetched = append(t.Fetched, key)
	if t.FetchErr != nil {
		return ticket.Ticket{}, t.FetchErr
	}
	tk := t.Ticket
	if tk.Identifier == "" {
		tk.Identifier = key
	}
	return tk, nil
}

func (t *FakeTracker) SelectNextTicket(...tracker.Key) (ticket.Ticket, bool, error) {
	return ticket.Ticket{}, false, nil
}

func (t *FakeTracker) MoveToInProgress(key tracker.Key) error {
	t.Claimed = append(t.Claimed, key)
	return t.ClaimErr
}

// StillReady reports a ticket claimed and not since released as taken, so a
// test that claims twice sees the second claim lost.
func (t *FakeTracker) StillReady(key tracker.Key) (bool, error) {
	held := 0
	for _, k := range t.Claimed {
		if k == key {
			held++
		}
	}
	for _, k := range t.Released {
		if k == key {
			held--
		}
	}
	return held <= 0, t.ReadyErr
}

func (t *FakeTracker) ReleaseToTodo(key tracker.Key) error {
	t.Released = append(t.Released, key)
	return t.ReleErr
}

func (t *FakeTracker) MoveToCanceled(key tracker.Key) error {
	t.Canceled = append(t.Canceled, key)
	return t.CancelErr
}

func (t *FakeTracker) ListInProgressClaims() ([]tracker.InProgressClaim, error) { return nil, nil }

func (t *FakeTracker) FileFinding(findings.Finding, tracker.FileFindingOptions) (tracker.CreatedIssue, error) {
	return tracker.CreatedIssue{}, nil
}

func (t *FakeTracker) SearchFindings(string) ([]tracker.ExistingFinding, error) { return nil, nil }

func (t *FakeTracker) RecordOccurrence(tracker.Key, tracker.Key) (int, error) { return 0, nil }

func (t *FakeTracker) AddComment(key tracker.Key, body string) error {
	t.Comments = append(t.Comments, body)
	return t.CommentEr
}
