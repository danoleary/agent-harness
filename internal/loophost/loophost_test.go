package loophost

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// narrator is the daemon's narration sink, recorded.
type narrator struct{ events []string }

func (n *narrator) Event(msg string)               { n.events = append(n.events, msg) }
func (n *narrator) Structured(r loopstream.Record) { n.events = append(n.events, r.Message) }

// queueTracker is hostio's scripted tracker with the two queue reads the daemon
// makes on top: the reaper's claim list and the selection its poll runs.
type queueTracker struct {
	*hostio.FakeTracker
	claims    []tracker.InProgressClaim
	claimsErr error
	next      ticket.Ticket
	nextOK    bool
	nextErr   error
}

func newQueueTracker() *queueTracker {
	return &queueTracker{FakeTracker: hostio.NewFakeTracker()}
}

func (t *queueTracker) ListInProgressClaims() ([]tracker.InProgressClaim, error) {
	return t.claims, t.claimsErr
}

func (t *queueTracker) SelectNextTicket() (ticket.Ticket, bool, error) {
	return t.next, t.nextOK, t.nextErr
}

// hostFor builds a Daemon over a throwaway project dir, the shape every test here
// wants: no credential, no Docker, no network.
func hostFor(t *testing.T, cfg config.Config) *Daemon {
	t.Helper()
	return NewDaemon(withProject(t, cfg), config.Loop{Name: "tickets"}, nil)
}

// sourceFor builds a TicketSource over the scripted tracker and a throwaway
// project dir.
func sourceFor(t *testing.T, cfg config.Config, client tracker.Tracker) *TicketSource {
	t.Helper()
	return NewTicketSource(withProject(t, cfg), &narrator{}, client)
}

func withProject(t *testing.T, cfg config.Config) config.Config {
	t.Helper()
	if cfg.ProjectPath == "" {
		cfg.ProjectPath = t.TempDir()
	}
	if cfg.StopFile == "" {
		cfg.StopFile = ".agent-harness/STOP"
	}
	return cfg
}

var (
	_ loop.Host     = (*Daemon)(nil)
	_ loop.Source   = (*TicketSource)(nil)
	_ loop.Pipeline = (*TicketPipeline)(nil)
)

// --- stop control -----------------------------------------------------------

// The sentinel is the operator's remote control, so where it lands matters: a
// relative STOP_FILE hangs off the Consumer checkout (the documented default), an
// absolute one is taken as given.
func TestStopFileResolvesRelativeToTheCheckout(t *testing.T) {
	project := t.TempDir()
	rel := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: ".agent-harness/STOP"}})
	if want := filepath.Join(project, ".agent-harness", "STOP"); rel.StopFile() != want {
		t.Errorf("stop file = %q, want %q (a relative override hangs off PROJECT_PATH)", rel.StopFile(), want)
	}

	abs := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: filepath.Join(t.TempDir(), "STOP")}})
	if !filepath.IsAbs(abs.StopFile()) || filepath.Dir(abs.StopFile()) == project {
		t.Errorf("stop file = %q, want the absolute override used as-is", abs.StopFile())
	}
}

// Startup clears a stale sentinel left by a prior run — and an absent one is
// success, or a fresh daemon would refuse to start on a clean host.
func TestClearStopFileRemovesTheSentinelAndToleratesItsAbsence(t *testing.T) {
	project := t.TempDir()
	stop := filepath.Join(project, "STOP")
	h := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: stop}})

	if err := h.ClearStopFile(); err != nil {
		t.Errorf("ClearStopFile() with no sentinel = %v, want nil (nothing to clear is success)", err)
	}
	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.ClearStopFile(); err != nil {
		t.Fatalf("ClearStopFile() = %v, want nil", err)
	}
	if _, err := os.Stat(stop); !os.IsNotExist(err) {
		t.Errorf("sentinel still present after ClearStopFile()")
	}
}

// The two stop signals fold into one predicate: a touched sentinel OR a SIGINT.
func TestStopRequestedFoldsTheSentinelAndTheSignal(t *testing.T) {
	project := t.TempDir()
	stop := filepath.Join(project, "STOP")
	h := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: stop}})

	if h.StopRequested() {
		t.Fatal("StopRequested() = true on a fresh daemon, want false")
	}
	if err := os.WriteFile(stop, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !h.StopRequested() {
		t.Error("StopRequested() = false with the sentinel present, want true")
	}
	if err := os.Remove(stop); err != nil {
		t.Fatal(err)
	}
	if h.StopRequested() {
		t.Fatal("StopRequested() = true after the sentinel was removed, want false")
	}
	h.RequestStop()
	if !h.StopRequested() {
		t.Error("StopRequested() = false after a SIGINT, want true")
	}
}

// Each loop has its own sentinel, STOP-<name>, so loops running side by side wind
// down one at a time; the plain STOP stops every loop. Startup clears both.
func TestEachLoopHasItsOwnStopSentinelAndPlainStopStopsAll(t *testing.T) {
	project := t.TempDir()
	cfg := withProject(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: ".agent-harness/STOP"}})
	prs := NewDaemon(cfg, config.Loop{Name: "pr-doctor"}, nil)
	tickets := NewDaemon(cfg, config.Loop{Name: "tickets"}, nil)
	own := filepath.Join(project, ".agent-harness", "STOP-pr-doctor")
	plain := filepath.Join(project, ".agent-harness", "STOP")
	if prs.LoopStopFile() != own {
		t.Fatalf("LoopStopFile = %q, want %q", prs.LoopStopFile(), own)
	}

	touch(t, own)
	if !prs.StopRequested() || tickets.StopRequested() {
		t.Errorf("STOP-pr-doctor must stop pr-doctor alone: prs %v, tickets %v", prs.StopRequested(), tickets.StopRequested())
	}
	touch(t, plain)
	if !tickets.StopRequested() {
		t.Error("the plain STOP must stop every loop")
	}
	if err := prs.ClearStopFile(); err != nil {
		t.Fatalf("ClearStopFile: %v", err)
	}
	if prs.StopRequested() {
		t.Error("startup must clear the loop's own sentinel and the plain one")
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- queue ------------------------------------------------------------------

// A selection failure comes back as an error, not an empty queue: the daemon
// still idles and re-polls on it, but must be able to say the read failed — the
// two used to look identical, so a dead token idled as "queue empty".
func TestNextReturnsASelectionFailureAsAnError(t *testing.T) {
	trk := newQueueTracker()
	trk.nextErr = errors.New("linear 503")
	h := sourceFor(t, config.Config{}, trk)

	_, claim, ok, err := h.Next()
	if ok || claim != nil || !errors.Is(err, trk.nextErr) {
		t.Errorf("Next() = (%v, %v, %v), want (nil, false, linear 503)", claim, ok, err)
	}
}

// The empty queue is not an error.
func TestNextEmptyQueueIsNotAnError(t *testing.T) {
	h := sourceFor(t, config.Config{}, newQueueTracker())
	if _, claim, ok, err := h.Next(); ok || claim != nil || err != nil {
		t.Errorf("Next() = (%v, %v, %v), want (nil, false, nil)", claim, ok, err)
	}
}

// CheckAccess fails on a rejected GH_TOKEN before touching the tracker, and on a
// tracker that rejects the read — naming the tracker so the operator knows which
// credential to fix.
func TestCheckAccessRejectsADeadTokenOrTracker(t *testing.T) {
	trk := newQueueTracker()
	var cfg config.Config
	cfg.Tracker.Kind = "linear"
	h := NewDaemon(withProject(t, cfg), config.Loop{Name: "tickets"}, sourceFor(t, cfg, trk).CheckAccess)
	dead := errors.New("GH_TOKEN check failed")
	h.checkToken = func() error { return dead }
	trk.nextErr = errors.New("HTTP 401")
	if err := h.CheckAccess(); !errors.Is(err, dead) {
		t.Errorf("CheckAccess() = %v, want the token failure, before the tracker is read", err)
	}

	h.checkToken = func() error { return nil }
	err := h.CheckAccess()
	if !errors.Is(err, trk.nextErr) || !strings.Contains(err.Error(), "linear tracker") {
		t.Errorf("CheckAccess() = %v, want the tracker failure naming the linear tracker", err)
	}

	trk.nextErr = nil
	if err := h.CheckAccess(); err != nil {
		t.Errorf("CheckAccess() = %v with both credentials good, want nil", err)
	}
}

// Claim-on-select (ADR-0003): a resolved ticket is claimed before the daemon runs
// it, so a concurrent selection can't grab the same one.
func TestNextClaimsTheSelectedTicket(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "PROJ-7"}, true
	h := sourceFor(t, config.Config{Project: config.Project{BranchPrefix: "fix"}}, trk)

	item, claim, ok, _ := h.Next()
	if !ok || claim.Key() != "PROJ-7" || !claim.Held() {
		t.Fatalf("Next() = (%v, %v), want a held lease on PROJ-7", claim, ok)
	}
	// The item names the ticket and the slugged feature branch the Stages push.
	if want := (loop.WorkItem{Key: "PROJ-7", Branch: "fix/proj-7"}); item != want {
		t.Errorf("item = %+v, want %+v", item, want)
	}
	if len(trk.Claimed) != 1 || trk.Claimed[0] != "PROJ-7" {
		t.Errorf("claimed %v, want [PROJ-7] — selection must claim (ADR-0003)", trk.Claimed)
	}
}

// The reaper's list is a straight projection of the Tracker port's claims, so the
// daemon never learns which tracker it is talking to.
func TestClaimsProjectsTheTrackersClaims(t *testing.T) {
	started := time.Date(2026, 7, 3, 22, 0, 0, 0, time.UTC)
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{
		{Identifier: "PROJ-1", StartedAt: started, HasLinkedPR: true},
		{Identifier: "PROJ-2", StartedAt: started},
	}
	h := sourceFor(t, config.Config{}, trk)

	got, err := h.Claims()
	if err != nil {
		t.Fatalf("Claims() error = %v", err)
	}
	want := []loop.StaleClaim{
		{Key: "PROJ-1", StartedAt: started, HasLinkedPR: true},
		{Key: "PROJ-2", StartedAt: started},
	}
	if len(got) != len(want) {
		t.Fatalf("claims = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("claim %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A list failure surfaces as an error the loop warns about and swallows — never a
// nil-claims lie that would read as "nothing stranded".
func TestClaimsSurfacesTheError(t *testing.T) {
	trk := newQueueTracker()
	trk.claimsErr = errors.New("linear boom")
	h := sourceFor(t, config.Config{}, trk)

	if _, err := h.Claims(); err == nil {
		t.Error("Claims() error = nil, want the tracker's failure surfaced")
	}
}

// The three ticket mutations are the daemon's whole write surface on the board:
// the reaper's release, and the close and comment it settles a run with.
func TestTicketMutationsReachTheTracker(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "PROJ-2"}, true
	h := sourceFor(t, config.Config{}, trk)

	if err := h.Release("PROJ-1"); err != nil {
		t.Fatalf("Release() = %v", err)
	}
	_, claim, _, _ := h.Next()
	if err := claim.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := h.Comment("PROJ-3", "breadcrumb"); err != nil {
		t.Fatalf("Comment() = %v", err)
	}
	if len(trk.Released) != 1 || trk.Released[0] != "PROJ-1" {
		t.Errorf("released %v, want [PROJ-1]", trk.Released)
	}
	// Canceled, not released: a superseded ticket must leave the selection AND the
	// reaper pool, or the reaper re-loops it past the TTL (BEH-682).
	if len(trk.Canceled) != 1 || trk.Canceled[0] != "PROJ-2" {
		t.Errorf("canceled %v, want [PROJ-2]", trk.Canceled)
	}
	if len(trk.Comments) != 1 || trk.Comments[0] != "breadcrumb" {
		t.Errorf("comments = %v, want [breadcrumb]", trk.Comments)
	}
}

// --- disk reclaim -----------------------------------------------------------

// A Consumer that declares no cache.prune_command must skip the rung entirely
// rather than shell out to someone else's package manager (BEH-641).
func TestCachePruneIsANoOpWhenNoCommandIsDeclared(t *testing.T) {
	h := hostFor(t, config.Config{})
	if err := h.CachePrune(); err != nil {
		t.Errorf("CachePrune() with nothing declared = %v, want nil (the rung is skipped)", err)
	}
}

// The declared command is run through a shell — a Consumer writes a command line,
// not an argv — with the checkout as its cwd.
func TestCachePruneRunsTheDeclaredCommand(t *testing.T) {
	project := t.TempDir()
	marker := filepath.Join(project, "pruned")
	h := hostFor(t, config.Config{Host: config.Host{ProjectPath: project}, Project: config.Project{CachePruneCommand: "echo ran > pruned"}})

	if err := h.CachePrune(); err != nil {
		t.Fatalf("CachePrune() = %v, want nil", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("declared prune command did not run in the checkout: %v", err)
	}
}

// A failing prune is an error the loop narrates and swallows — reclaim is never a
// ticket outcome — so the error must carry the command's own output to be useful.
func TestCachePruneSurfacesTheFailureOutput(t *testing.T) {
	h := hostFor(t, config.Config{Project: config.Project{CachePruneCommand: "echo nope >&2; exit 3"}})

	err := h.CachePrune()
	if err == nil {
		t.Fatal("CachePrune() = nil, want the non-zero exit surfaced")
	}
	if got := err.Error(); !strings.Contains(got, "nope") {
		t.Errorf("error = %q, want it to carry the command's output", got)
	}
}

// parsePrunedCount reads the removed-worktree count from the prune script's
// authoritative summary line ("Pruned N worktree(s).") — the count the loop
// narrates — tolerating the per-removal and tip lines around it, and degrading to
// 0 when the script pruned nothing or printed no summary.
func TestParsePrunedCount(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   int
	}{
		{
			name: "two removed",
			output: "Merged + clean worktree(s) eligible for removal:\n" +
				"  • /h/.claude/worktrees/a\n" +
				"  • /h/.claude/worktrees/b\n" +
				"removed /h/.claude/worktrees/a\n" +
				"removed /h/.claude/worktrees/b\n" +
				"Pruned 2 worktree(s). Tip: 'pnpm store prune' frees orphaned content in the global pnpm store too.\n",
			want: 2,
		},
		{
			name:   "nothing to prune",
			output: "Nothing to prune — no merged + clean worktree under /h/.claude/worktrees.\n",
			want:   0,
		},
		{
			name:   "empty",
			output: "",
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePrunedCount(tc.output); got != tc.want {
				t.Errorf("parsePrunedCount(%q) = %d, want %d", tc.output, got, tc.want)
			}
		})
	}
}

// A missing prune script is an ordinary reclaim failure: an error the loop warns
// about and swallows, never a crash or a phantom count.
func TestPruneMergedWorktreesSurfacesAMissingScript(t *testing.T) {
	h := hostFor(t, config.Config{})

	removed, err := h.PruneMergedWorktrees()
	if err == nil {
		t.Fatal("PruneMergedWorktrees() error = nil, want the missing script surfaced")
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 when the prune never ran", removed)
	}
}

// The PR loop keys its lease on the branch (ADR-0019), so it asks the ticket loop
// which branches it holds: every agent-claimed In Progress ticket's slugged branch.
func TestHeldBranchesMapsEachClaimToItsBranch(t *testing.T) {
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{{Identifier: "#7"}, {Identifier: "PROJ-2"}}
	h := sourceFor(t, config.Config{Project: config.Project{BranchPrefix: "feat"}}, trk)

	got, err := h.HeldBranches()
	if err != nil {
		t.Fatalf("HeldBranches() error = %v", err)
	}
	if len(got) != 2 || got["feat/gh-7"] != "#7" || got["feat/proj-2"] != "PROJ-2" {
		t.Errorf("held = %v, want feat/gh-7 → #7 and feat/proj-2 → PROJ-2", got)
	}

	trk.claimsErr = errors.New("tracker down")
	if _, err := h.HeldBranches(); err == nil {
		t.Error("a failed claim read must be returned, not read as nothing held")
	}
}

// The ticket Pipeline runs the loop's Steps: the built-in ticket loop runs all
// three Stages, and a loop that leaves out retro runs no retrospective.
func TestTicketDepsFollowTheLoopsSteps(t *testing.T) {
	builtIn, _ := config.Project{}.LoopNamed("")
	if d := ticketDeps(builtIn, hostio.NewFake(), config.Config{}, nil, stages.Args{}); d.Implementation == nil || d.Review == nil || d.Retrospective == nil {
		t.Errorf("the built-in ticket loop runs all three Stages, got %+v", d)
	}
	noRetro := builtIn
	noRetro.Steps = noRetro.Steps[:2]
	if d := ticketDeps(noRetro, hostio.NewFake(), config.Config{}, nil, stages.Args{}); d.Retrospective != nil {
		t.Error("a loop without retro must run no retrospective")
	}
}
