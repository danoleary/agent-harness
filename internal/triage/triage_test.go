package triage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// fakeBacklog is a tracker backlog that records what the loop did to it.
type fakeBacklog struct {
	tickets  []ticket.Ticket
	listErr  error
	readyErr error
	commErr  error
	ready    []string
	comments map[string][]string
}

func (f *fakeBacklog) Untriaged() ([]ticket.Ticket, error) { return f.tickets, f.listErr }
func (f *fakeBacklog) MarkReady(key tracker.Key) error {
	if f.readyErr != nil {
		return f.readyErr
	}
	f.ready = append(f.ready, key)
	return nil
}
func (f *fakeBacklog) AddComment(key tracker.Key, body string) error {
	if f.commErr != nil {
		return f.commErr
	}
	if f.comments == nil {
		f.comments = map[string][]string{}
	}
	f.comments[key] = append(f.comments[key], body)
	return nil
}

// fakeSandbox scripts a Triage session: it writes verdict (when not empty) into
// the dropbox the run mounts, and returns outcome.
type fakeSandbox struct {
	preflight error
	verdict   string
	outcome   session.Outcome
	runs      []hostio.AgentRun
}

func (s *fakeSandbox) Preflight() error { return s.preflight }
func (s *fakeSandbox) Agent(a hostio.AgentRun) hostio.Result {
	s.runs = append(s.runs, a)
	if s.verdict != "" {
		_ = os.WriteFile(filepath.Join(a.FindingsDir, "verdict.json"), []byte(s.verdict), 0o644)
	}
	return hostio.Result{Outcome: s.outcome}
}

var bug = ticket.Ticket{Identifier: "#7", Title: "Fix the flag", Description: "The --dry-run flag is ignored by `start`."}

func triageLoop(t *testing.T) config.Loop {
	t.Helper()
	l, ok := config.Project{}.LoopNamed(config.TriageLoopName)
	if !ok {
		t.Fatal("no built-in triage loop")
	}
	return l
}

// pipeline is the triage Pipeline over a temp checkout, with sb as its sandbox.
func pipeline(t *testing.T, l config.Loop, b *fakeBacklog, sb *fakeSandbox) Pipeline {
	t.Helper()
	dir := t.TempDir()
	var cfg config.Config
	cfg.ProjectPath = dir
	cfg.Tracker.Kind = "github"
	cfg.Tracker.ReadyLabel = "ready-for-agent"
	return Pipeline{
		Loop: l, Cfg: cfg, Backlog: b, Store: StoreFor(dir),
		NewSandbox: func(config.Config, *runlog.Logger) Sandbox { return sb },
	}
}

func run(p Pipeline, t ticket.Ticket) stages.Result {
	return p.Run(loop.WorkItem{Key: t.Identifier, Context: Item{Ticket: t, Fingerprint: Fingerprint(t)}}, nil)
}

// A ready verdict marks the ticket ready, notes why, and is recorded, so the next
// poll does not judge the ticket again.
func TestAReadyVerdictMarksTheTicketReady(t *testing.T) {
	b := &fakeBacklog{tickets: []ticket.Ticket{bug}}
	sb := &fakeSandbox{verdict: `{"ready": true, "reason": "Clear bug with a named flag.", "questions": []}`}
	p := pipeline(t, triageLoop(t), b, sb)

	res := run(p, bug)
	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("result = %+v, want a Shipped verdict", res)
	}
	if len(b.ready) != 1 || b.ready[0] != "#7" {
		t.Errorf("ready = %v, want #7 marked ready", b.ready)
	}
	if c := b.comments["#7"]; len(c) != 1 || !strings.Contains(c[0], "Clear bug with a named flag.") {
		t.Errorf("comments = %v, want the reason noted", c)
	}
	if !strings.Contains(sb.runs[0].Prompt, "The --dry-run flag is ignored") {
		t.Error("the session prompt must carry the ticket")
	}
	if _, _, ok, _ := NewSource(b, p.Store).Next(); ok {
		t.Error("a judged ticket must not be yielded again")
	}
}

// A not-ready verdict leaves the label alone and comments the questions, and an
// edit to the ticket brings it back for another judgement.
func TestANotReadyVerdictCommentsTheQuestions(t *testing.T) {
	b := &fakeBacklog{tickets: []ticket.Ticket{bug}}
	sb := &fakeSandbox{verdict: `{"ready": false, "reason": "Two behaviours fit.", "questions": ["Should --dry-run print the plan or exit 0 silently?"]}`}
	p := pipeline(t, triageLoop(t), b, sb)

	if res := run(p, bug); res.Disposition != stages.Shipped {
		t.Fatalf("result = %+v: a not-ready verdict is a verdict reached, not a failure", res)
	}
	if len(b.ready) != 0 {
		t.Errorf("ready = %v, want nothing marked", b.ready)
	}
	c := b.comments["#7"]
	if len(c) != 1 || !strings.Contains(c[0], "- Should --dry-run print the plan") || !strings.Contains(c[0], "`ready-for-agent`") {
		t.Fatalf("comments = %v, want the question and the label to add", c)
	}

	src := NewSource(b, p.Store)
	if _, _, ok, _ := src.Next(); ok {
		t.Error("an unedited ticket must not be judged again")
	}
	edited := bug
	edited.Description += "\n\nAnswer: print the plan."
	b.tickets = []ticket.Ticket{edited}
	if item, _, ok, _ := src.Next(); !ok || item.Key != "#7" {
		t.Error("an edited ticket must be judged again")
	}
}

// Without an apply step the loop judges and records, and touches no ticket.
func TestWithoutApplyTheVerdictIsOnlyRecorded(t *testing.T) {
	l := triageLoop(t)
	l.Steps = l.Steps[:1]
	b := &fakeBacklog{tickets: []ticket.Ticket{bug}}
	p := pipeline(t, l, b, &fakeSandbox{verdict: `{"ready": true, "reason": "ok", "questions": []}`})

	if res := run(p, bug); res.Disposition != stages.Shipped {
		t.Fatalf("result = %+v", res)
	}
	if len(b.ready) != 0 || len(b.comments) != 0 {
		t.Errorf("ready = %v, comments = %v: a loop without apply must not touch the tracker", b.ready, b.comments)
	}
	if !p.Store.Judged("#7", Fingerprint(bug)) {
		t.Error("the verdict must be recorded")
	}
}

// A run that reaches no usable verdict, or cannot act on it, records nothing and
// counts as a failure, so the next poll tries again and a run of them trips the
// breaker.
func TestARunWithNoVerdictActedOnRecordsNothing(t *testing.T) {
	cases := []struct {
		name    string
		verdict string
		backlog fakeBacklog
	}{
		{"no file", "", fakeBacklog{}},
		{"not json", "ready", fakeBacklog{}},
		{"no ready field", `{"reason": "x"}`, fakeBacklog{}},
		{"not ready without questions", `{"ready": false, "reason": "vague", "questions": [" "]}`, fakeBacklog{}},
		{"label refused", `{"ready": true, "reason": "ok"}`, fakeBacklog{readyErr: errors.New("403")}},
		{"comment refused", `{"ready": false, "reason": "x", "questions": ["q"]}`, fakeBacklog{commErr: errors.New("403")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.backlog
			p := pipeline(t, triageLoop(t), &b, &fakeSandbox{verdict: tc.verdict})
			if res := run(p, bug); res.OK || res.Disposition == stages.Shipped {
				t.Errorf("result = %+v, want a failure", res)
			}
			if p.Store.Judged("#7", Fingerprint(bug)) {
				t.Error("no verdict may be recorded")
			}
		})
	}
}

// A spending cap or a failed preflight is reported as such, for the daemon's
// backoff, and records nothing.
func TestCapAndPreflightAreReported(t *testing.T) {
	p := pipeline(t, triageLoop(t), &fakeBacklog{}, &fakeSandbox{outcome: session.Outcome{SpendingCapAbort: true}})
	if res := run(p, bug); res.Disposition != stages.CapAborted {
		t.Errorf("cap: result = %+v, want CapAborted", res)
	}
	sb := &fakeSandbox{preflight: errors.New("docker down")}
	p = pipeline(t, triageLoop(t), &fakeBacklog{}, sb)
	if res := run(p, bug); res.Disposition != stages.PreflightAborted || len(sb.runs) != 0 {
		t.Errorf("preflight: result = %+v, runs = %d, want PreflightAborted and no session", res, len(sb.runs))
	}
}

// The Source yields the first untriaged ticket, holds no claim on it, and reports
// a failed read as an error rather than an empty backlog.
func TestSourceYieldsUnjudgedTicketsWithoutClaiming(t *testing.T) {
	b := &fakeBacklog{tickets: []ticket.Ticket{bug, {Identifier: "#8", Title: "other"}}}
	store := StoreFor(t.TempDir())
	if err := store.Save("#7", Record{Fingerprint: Fingerprint(bug)}); err != nil {
		t.Fatal(err)
	}
	item, claim, ok, err := NewSource(b, store).Next()
	if err != nil || !ok || item.Key != "#8" {
		t.Fatalf("Next = %+v, %v, %v; want #8 (#7 is judged)", item, ok, err)
	}
	if claim.Held() {
		t.Error("triage must not claim the ticket")
	}

	b.listErr = errors.New("401")
	if _, _, _, err := NewSource(b, store).Next(); err == nil {
		t.Error("a failed backlog read must be an error")
	}
}

// Jira has no Backlog, so a triage loop over it is refused with the reason.
func TestAsBacklogRefusesATrackerWithoutOne(t *testing.T) {
	var t0 tracker.Tracker
	if _, err := AsBacklog(t0, "jira"); err == nil || !strings.Contains(err.Error(), "linear, github") {
		t.Errorf("err = %v, want a refusal naming the supported trackers", err)
	}
}
