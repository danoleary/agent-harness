package loophost

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/hostlock"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/ship"
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

// SelectNextTicket offers the scripted ticket, unless the caller skips it.
func (t *queueTracker) SelectNextTicket(skip ...tracker.Key) (ticket.Ticket, bool, error) {
	for _, k := range skip {
		if t.nextOK && k == t.next.Identifier {
			return ticket.Ticket{}, false, nil
		}
	}
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
	// Its Context is the ticket as selection read it (#330).
	if want := (loop.WorkItem{Key: "PROJ-7", Branch: "fix/proj-7", Context: nowork.Of(trk.next)}); item != want {
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

// The four ticket mutations are the daemon's whole write surface on the board:
// the reaper's release, and the close, the blocked label and the comment it
// settles a run with.
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
	if err := h.Block("PROJ-4"); err != nil {
		t.Fatalf("Block() = %v", err)
	}
	if len(trk.Blocked) != 1 || trk.Blocked[0] != "PROJ-4" {
		t.Errorf("blocked %v, want [PROJ-4]", trk.Blocked)
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

// A ticket the daemon gives back goes back to Todo, and the stream says so
// (BEH-590, BEH-677) — the wording the PR loop must not borrow (#173).
func TestReleasingNarratesTheTicketGoingBackToTodo(t *testing.T) {
	h := sourceFor(t, config.Config{}, newQueueTracker())
	for why, want := range map[loop.ReleaseReason]string{
		loop.ReleaseNoPR:     "PROJ-1 produced no PR; releasing back to Todo so it isn't stranded In Progress (BEH-590)",
		loop.ReleaseCapAbort: "spending-cap abort on PROJ-1; releasing to Todo",
		loop.ReleaseStale:    "reaping stale claim PROJ-1 (In Progress past TTL with no branch/PR); releasing to Todo (BEH-677)",
	} {
		if got := h.Releasing(why, "PROJ-1"); got != want {
			t.Errorf("Releasing(%d) = %q, want %q", why, got, want)
		}
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
// which branches it holds: the slugged branch of every agent-claimed In Progress
// ticket whose run is live, holding its run lock (#231). A ticket stays In
// Progress once its PR is open, but from the moment its run ends its red CI is the
// PR loop's (#110), whether or not a worktree was left behind.
func TestHeldBranchesMapsEachLiveClaimToItsBranch(t *testing.T) {
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{{Identifier: "#7"}, {Identifier: "PROJ-2"}, {Identifier: "PROJ-3", HasLinkedPR: true}}
	h := sourceFor(t, config.Config{Project: config.Project{BranchPrefix: "feat"}}, trk)
	for _, slug := range []string{"gh-7", "proj-2", "proj-3"} {
		if err := os.MkdirAll(filepath.Join(h.cfg.ProjectPath, ".claude", "worktrees", slug), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"#7", "PROJ-2"} {
		defer HoldRun(h.cfg, key, &narrator{})()
	}

	got, err := h.HeldBranches()
	if err != nil {
		t.Fatalf("HeldBranches() error = %v", err)
	}
	if len(got) != 2 || got["feat/gh-7"] != "#7" || got["feat/proj-2"] != "PROJ-2" {
		t.Errorf("held = %v, want feat/gh-7 → #7 and feat/proj-2 → PROJ-2, and not PROJ-3, whose run is over though its worktree is left", got)
	}

	trk.claimsErr = errors.New("tracker down")
	if _, err := h.HeldBranches(); err == nil {
		t.Error("a failed claim read must be returned, not read as nothing held")
	}
}

// #231, replaying herd BEH-1301 / herd#1386: a loop restart interrupted the review
// stage's CI watch, so no retrospective ran and the worktree stayed, clean, with
// the ticket In Progress. The run that held the branch is dead, so the branch is
// free for the PR loop, which reclaims the worktree.
func TestARunThatEndedNoLongerHoldsItsBranch(t *testing.T) {
	repo := gittest.New(t)
	repo.Handoff("beh-1301")
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{{Identifier: "BEH-1301", HasLinkedPR: true}}
	src := sourceFor(t, config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}, Host: config.Host{ProjectPath: repo.Dir}}, trk)

	release := HoldRun(src.cfg, "BEH-1301", &narrator{})
	if held, _ := src.HeldBranches(); held["feat/beh-1301"] != "BEH-1301" {
		t.Fatalf("held = %v, want feat/beh-1301 held by BEH-1301 while its run is live", held)
	}
	release()

	held, err := src.HeldBranches()
	if err != nil {
		t.Fatalf("HeldBranches() error = %v", err)
	}
	if by, ok := held["feat/beh-1301"]; ok {
		t.Errorf("feat/beh-1301 still held by %s after its run ended, so the PR loop skips herd#1386 forever", by)
	}
}

// The PR loop's liveness probe takes the run lock for an instant. A run that
// starts in that instant waits it out and holds the lock, rather than running its
// whole length unheld for the PR loop to mistake for dead.
func TestHoldRunWaitsOutALivenessProbe(t *testing.T) {
	cfg := withProject(t, config.Config{})
	probe, ok, err := hostlock.File(config.RunLockFile(cfg.ProjectPath, "beh-2")).TryLock()
	if err != nil || !ok {
		t.Fatalf("TryLock() = ok %v err %v", ok, err)
	}
	go func() { time.Sleep(20 * time.Millisecond); probe() }()
	log := &narrator{}
	release := HoldRun(cfg, "BEH-2", log)
	defer release()
	if len(log.events) != 0 {
		t.Errorf("events = %v, want the run lock taken once the probe let go", log.events)
	}
	if held, _ := hostlock.File(config.RunLockFile(cfg.ProjectPath, "beh-2")).Held(); !held {
		t.Error("the run must hold its lock after a probe let go")
	}
}

// A second run of a ticket whose run lock is taken still runs: the lock is a
// liveness signal for other loops, not a gate on this one, and the live holder
// keeps the branch held.
func TestHoldRunNarratesAndRunsWhenTheLockIsTaken(t *testing.T) {
	cfg := withProject(t, config.Config{})
	defer HoldRun(cfg, "BEH-1", &narrator{})()
	log := &narrator{}
	HoldRun(cfg, "BEH-1", log)()
	if len(log.events) != 1 || !strings.Contains(log.events[0], "BEH-1") {
		t.Errorf("events = %v, want one warning naming BEH-1", log.events)
	}
	if held, _ := hostlock.File(config.RunLockFile(cfg.ProjectPath, "beh-1")).Held(); !held {
		t.Error("the first run's lock must survive the second run letting go of nothing")
	}
}

// The ticket Pipeline maps a committed-fix recovery onto the Disposition the loop
// branches on: a landed branch is Shipped, a refused one NeedsHuman with the reason
// (#230), and one that could not land the plain no-PR the release handles.
func TestRecoveredMapsTheRecoveryOntoADisposition(t *testing.T) {
	cases := []struct {
		rec  ship.Recovery
		want stages.Result
	}{
		{ship.Recovery{Attempted: true, Shipped: true}, stages.Result{OK: true, Disposition: stages.Shipped}},
		{ship.Recovery{Attempted: true, Refused: `the host gate "unit" was red (exit 1)`}, stages.Result{Disposition: stages.NeedsHuman, Refusal: `the host gate "unit" was red (exit 1)`}},
		{ship.Recovery{Attempted: true}, stages.Result{}},
	}
	for _, tc := range cases {
		if got := recovered(tc.rec); got != tc.want {
			t.Errorf("recovered(%+v) = %+v, want %+v", tc.rec, got, tc.want)
		}
	}
}

// The ticket Pipeline runs the loop's Steps: the built-in ticket loop runs all
// three Stages, and a loop that leaves out retro runs no retrospective.
func TestTicketDepsFollowTheLoopsSteps(t *testing.T) {
	builtIn, _ := config.Project{}.LoopNamed("")
	if d := ticketDeps(builtIn, config.Config{}, nil, "run", stages.Args{}); d.Implementation == nil || d.Review == nil || d.Retrospective == nil {
		t.Errorf("the built-in ticket loop runs all three Stages, got %+v", d)
	}
	noRetro := builtIn
	noRetro.Steps = noRetro.Steps[:2]
	if d := ticketDeps(noRetro, config.Config{}, nil, "run", stages.Args{}); d.Retrospective != nil {
		t.Error("a loop without retro must run no retrospective")
	}
}

// #110: the stage args follow the loop's Steps. The built-in ticket loop watches
// its PR's CI; one that leaves out watch_ci hands CI to the PR loop.
func TestTicketArgsLeaveCIToThePRLoopWithoutWatchCI(t *testing.T) {
	builtIn, _ := config.Project{}.LoopNamed("")
	if a := ticketArgs(builtIn, loop.WorkItem{Key: "PROJ-1"}, nil); a.Identifier != "PROJ-1" || a.LeaveCIToPRLoop {
		t.Errorf("the built-in ticket loop watches CI in its review stage, got %+v", a)
	}
	noWatch := builtIn
	noWatch.Steps = []config.Step{{Use: config.StepImplement}, {Use: config.StepReview}, {Use: config.StepRetro}}
	if a := ticketArgs(noWatch, loop.WorkItem{Key: "PROJ-1"}, nil); !a.LeaveCIToPRLoop {
		t.Error("a ticket loop without watch_ci must leave CI to the PR loop")
	}
}

// Instances of one loop (#89): STOP-<id> stops that instance alone, the loop's
// STOP-<name> and the plain STOP stop every instance.
func TestAnInstanceStopsOnItsOwnSentinelAndOnEveryShared(t *testing.T) {
	project := t.TempDir()
	cfg := withProject(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: ".agent-harness/STOP"}})
	a := NewDaemon(cfg, config.Loop{Name: "tickets", Instance: "a"}, nil)
	b := NewDaemon(cfg, config.Loop{Name: "tickets", Instance: "b"}, nil)
	dir := filepath.Join(project, ".agent-harness")

	touch(t, filepath.Join(dir, "STOP-b"))
	if !b.StopRequested() || a.StopRequested() {
		t.Errorf("STOP-b must stop b alone: a %v, b %v", a.StopRequested(), b.StopRequested())
	}
	os.Remove(filepath.Join(dir, "STOP-b"))
	for _, shared := range []string{"STOP-tickets", "STOP"} {
		touch(t, filepath.Join(dir, shared))
		if !a.StopRequested() || !b.StopRequested() {
			t.Errorf("%s must stop every instance: a %v, b %v", shared, a.StopRequested(), b.StopRequested())
		}
		os.Remove(filepath.Join(dir, shared))
	}
}

// A starting instance clears its own stale sentinel. It leaves the shared ones
// alone while another instance is running, or it would swallow a STOP that
// instance has not reached its next checkpoint to see.
func TestStartupLeavesASharedStopForARunningInstance(t *testing.T) {
	project := t.TempDir()
	cfg := withProject(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: ".agent-harness/STOP"}})
	dir := filepath.Join(project, ".agent-harness")
	b := NewDaemon(cfg, config.Loop{Name: "tickets", Instance: "b"}, nil)

	touch(t, filepath.Join(dir, "STOP"))
	touch(t, filepath.Join(dir, "STOP-b"))
	// Instance a is running: its pidfile names a live process that is not this one.
	if err := os.WriteFile(filepath.Join(dir, "loop-a.pid"), []byte(strconv.Itoa(os.Getppid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.ClearStopFile(); err != nil {
		t.Fatalf("ClearStopFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "STOP-b")); !os.IsNotExist(err) {
		t.Error("an instance must clear its own stale sentinel at startup")
	}
	if _, err := os.Stat(filepath.Join(dir, "STOP")); err != nil {
		t.Error("the plain STOP is instance a's to act on; b's startup must not clear it")
	}

	// With no other daemon alive, a leftover shared sentinel is stale: cleared.
	if err := os.WriteFile(filepath.Join(dir, "loop-a.pid"), []byte("999999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.ClearStopFile(); err != nil {
		t.Fatalf("ClearStopFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "STOP")); !os.IsNotExist(err) {
		t.Error("a shared sentinel no running daemon can act on must be cleared, or it stops the fresh one")
	}
}
