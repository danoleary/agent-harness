package hostio

import (
	"fmt"
	"path/filepath"

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
// [NewFake] returns a *healthy* host — preflight green, worktree present and
// clean, branch ahead of main with a non-empty diff, sessions exiting 0 with a
// review verdict emitted, gh answering — so a test overrides only the one signal
// it is about and every other field stays out of its way.
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

	// --- repo state ---

	Ahead          int
	Exists         bool
	Clean          bool
	DiffEmpty      bool
	DocsOnly       bool
	BranchThere    bool
	Pushed         bool
	Disjoint       bool
	Rebased        bool
	Subjects       []string
	Head           string
	RebaseVerdict  gitpkg.RebaseResult
	Resolved       string
	ResumedAdvice  string
	AlreadyOnMain  bool
	CreateErr      error
	RemoveErr      error
	StripErr       error
	CheckpointErr  error
	RegraftErr     error
	HeadErr        error
	RerunCommitErr error

	// --- remote state ---

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
	Filed   []string
	Routed  []string
	Calls   []string
	Removed []string
}

var _ Host = (*Fake)(nil)

// NewFake returns a Fake scripted as a healthy host on the happy path.
func NewFake() *Fake {
	return &Fake{
		AgentOutcome:  session.Outcome{ReviewVerdictEmitted: true},
		Ahead:         1,
		Exists:        true,
		Clean:         true,
		BranchThere:   true,
		Pushed:        true,
		Rebased:       true,
		Subjects:      []string{"feat: do the thing"},
		Head:          "abc1234",
		RebaseVerdict: gitpkg.RebaseClean,
		PRURL:         "https://github.com/acme/widgets/pull/1",
		PRThere:       true,
		OpenPRThere:   true,
		ChecksOK:      true,
		CI:            ci.Outcome{OK: true, Reason: "all checks green"},
		Trk:           NewFakeTracker(),
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
	return []string{"run", "--name", "fake-" + s.Label, "bash", "-lc", s.Command}
}

// --- repo ---

func (f *Fake) WorktreePath(slug string) string { return filepath.Join("/fake/worktrees", slug) }
func (f *Fake) BranchName(slug string) string   { return "feat/" + slug }
func (f *Fake) WorktreeExists(string) bool      { return f.Exists }

func (f *Fake) CreateWorktree(slug string) error {
	f.record("create-worktree %s", slug)
	if f.CreateErr == nil {
		f.Exists = true
	}
	return f.CreateErr
}

func (f *Fake) RemoveWorktree(slug string) error {
	f.record("remove-worktree %s", slug)
	if f.RemoveErr == nil {
		f.Removed = append(f.Removed, slug)
	}
	return f.RemoveErr
}

func (f *Fake) WorktreeClean(string) bool { return f.Clean }

func (f *Fake) StripHandoffPaths(slug string) error { f.record("strip %s", slug); return f.StripErr }

func (f *Fake) Checkpoint(slug, key, stage string) error {
	f.record("checkpoint %s %s", key, stage)
	return f.CheckpointErr
}

func (f *Fake) HeadSHA(string) (string, error) { return f.Head, f.HeadErr }

func (f *Fake) EnsureCIRerunCommit(slug, headBefore string) error {
	f.record("ci-rerun-commit %s", headBefore)
	return f.RerunCommitErr
}

func (f *Fake) CommitsAhead(string) int        { return f.Ahead }
func (f *Fake) BranchDisjoint(string) bool     { return f.Disjoint }
func (f *Fake) BranchDiffEmpty(string) bool    { return f.DiffEmpty }
func (f *Fake) BranchDocsOnly(string) bool     { return f.DocsOnly }
func (f *Fake) BranchExists(string) bool       { return f.BranchThere }
func (f *Fake) BranchPushed(string) bool       { return f.Pushed }
func (f *Fake) CommitSubjects(string) []string { return f.Subjects }

func (f *Fake) Rebase(slug string) gitpkg.RebaseResult {
	f.record("rebase %s", slug)
	return f.RebaseVerdict
}

func (f *Fake) AbortRebase(slug string)                     { f.record("abort-rebase %s", slug) }
func (f *Fake) IsDisjoint(string) bool                      { return f.Disjoint }
func (f *Fake) IsRebased(string) bool                       { return f.Rebased }
func (f *Fake) Regraft(slug string) error                   { f.record("regraft %s", slug); return f.RegraftErr }
func (f *Fake) TicketAlreadyOnMain(string) bool             { return f.AlreadyOnMain }
func (f *Fake) ResolvedAdvisory(string, string) string      { return f.Resolved }
func (f *Fake) ResumedBranchAdvisory(string, string) string { return f.ResumedAdvice }

// --- remote ---

func (f *Fake) FetchMain() error { f.record("fetch-main"); return f.FetchErr }

func (f *Fake) Push(slug string) error { f.record("push %s", slug); return f.PushErr }

func (f *Fake) PushForceWithLease(slug string) error {
	f.record("push-force %s", slug)
	return f.ForcePushErr
}

func (f *Fake) CreatePR(slug, title, body string) (string, error) {
	f.record("create-pr %s", title)
	if f.CreatePRErr != nil {
		return "", f.CreatePRErr
	}
	return f.PRURL, nil
}

func (f *Fake) PRExists(string) bool           { return f.PRThere }
func (f *Fake) OpenPRExists(string) bool       { return f.OpenPRThere }
func (f *Fake) ChecksReadable() (bool, string) { return f.ChecksOK, f.ChecksDetail }

func (f *Fake) WatchCI(w CIWatch) ci.Outcome {
	f.record("watch-ci %s", w.Slug)
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

func (t *FakeTracker) SelectNextTicket() (ticket.Ticket, bool, error) {
	return ticket.Ticket{}, false, nil
}

func (t *FakeTracker) MoveToInProgress(key tracker.Key) error {
	t.Claimed = append(t.Claimed, key)
	return t.ClaimErr
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
