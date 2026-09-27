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

// hostFor builds a Real over a throwaway project dir, the shape every test here
// wants: no tracker credential, no Docker, no network.
func hostFor(t *testing.T, cfg config.Config, client tracker.Tracker) (*Real, *narrator) {
	t.Helper()
	if cfg.ProjectPath == "" {
		cfg.ProjectPath = t.TempDir()
	}
	if cfg.StopFile == "" {
		cfg.StopFile = ".agent-harness/STOP"
	}
	n := &narrator{}
	return New(cfg, n, client), n
}

var _ loop.Host = (*Real)(nil)

// --- stop control -----------------------------------------------------------

// The sentinel is the operator's remote control, so where it lands matters: a
// relative STOP_FILE hangs off the Consumer checkout (the documented default), an
// absolute one is taken as given.
func TestStopFileResolvesRelativeToTheCheckout(t *testing.T) {
	project := t.TempDir()
	rel, _ := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: ".agent-harness/STOP"}}, nil)
	if want := filepath.Join(project, ".agent-harness", "STOP"); rel.StopFile() != want {
		t.Errorf("stop file = %q, want %q (a relative override hangs off PROJECT_PATH)", rel.StopFile(), want)
	}

	abs, _ := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: filepath.Join(t.TempDir(), "STOP")}}, nil)
	if !filepath.IsAbs(abs.StopFile()) || filepath.Dir(abs.StopFile()) == project {
		t.Errorf("stop file = %q, want the absolute override used as-is", abs.StopFile())
	}
}

// Startup clears a stale sentinel left by a prior run — and an absent one is
// success, or a fresh daemon would refuse to start on a clean host.
func TestClearStopFileRemovesTheSentinelAndToleratesItsAbsence(t *testing.T) {
	project := t.TempDir()
	stop := filepath.Join(project, "STOP")
	h, _ := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: stop}}, nil)

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
	h, _ := hostFor(t, config.Config{Host: config.Host{ProjectPath: project, StopFile: stop}}, nil)

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

// --- queue ------------------------------------------------------------------

// A selection or claim failure must fold into "no ticket": the daemon idles and
// re-polls rather than dying on one bad poll — a tracker hiccup at 3am cannot be
// allowed to end an unattended run.
func TestResolveNextFoldsASelectionFailureIntoAnEmptyQueue(t *testing.T) {
	trk := newQueueTracker()
	trk.nextErr = errors.New("linear 503")
	h, _ := hostFor(t, config.Config{}, trk)

	if id, ok := h.ResolveNext(); ok || id != "" {
		t.Errorf("ResolveNext() = (%q, %v), want (\"\", false) so the daemon idles and re-polls", id, ok)
	}
}

// Claim-on-select (ADR-0003): a resolved ticket is claimed before the daemon runs
// it, so a concurrent selection can't grab the same one.
func TestResolveNextClaimsTheSelectedTicket(t *testing.T) {
	trk := newQueueTracker()
	trk.next, trk.nextOK = ticket.Ticket{Identifier: "PROJ-7"}, true
	h, _ := hostFor(t, config.Config{}, trk)

	id, ok := h.ResolveNext()
	if !ok || id != "PROJ-7" {
		t.Fatalf("ResolveNext() = (%q, %v), want (\"PROJ-7\", true)", id, ok)
	}
	if len(trk.Claimed) != 1 || trk.Claimed[0] != "PROJ-7" {
		t.Errorf("claimed %v, want [PROJ-7] — selection must claim (ADR-0003)", trk.Claimed)
	}
}

// The reaper's list is a straight projection of the Tracker port's claims, so the
// daemon never learns which tracker it is talking to.
func TestListInProgressClaimsProjectsTheTrackersClaims(t *testing.T) {
	started := time.Date(2026, 7, 3, 22, 0, 0, 0, time.UTC)
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{
		{Identifier: "PROJ-1", StartedAt: started, HasLinkedPR: true},
		{Identifier: "PROJ-2", StartedAt: started},
	}
	h, _ := hostFor(t, config.Config{}, trk)

	got, err := h.ListInProgressClaims()
	if err != nil {
		t.Fatalf("ListInProgressClaims() error = %v", err)
	}
	want := []loop.StaleClaim{
		{Identifier: "PROJ-1", StartedAt: started, HasLinkedPR: true},
		{Identifier: "PROJ-2", StartedAt: started},
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
func TestListInProgressClaimsSurfacesTheError(t *testing.T) {
	trk := newQueueTracker()
	trk.claimsErr = errors.New("linear boom")
	h, _ := hostFor(t, config.Config{}, trk)

	if _, err := h.ListInProgressClaims(); err == nil {
		t.Error("ListInProgressClaims() error = nil, want the tracker's failure surfaced")
	}
}

// The three ticket mutations are the daemon's whole write surface on the board.
func TestTicketMutationsReachTheTracker(t *testing.T) {
	trk := newQueueTracker()
	h, _ := hostFor(t, config.Config{}, trk)

	if err := h.ReleaseTicket("PROJ-1"); err != nil {
		t.Fatalf("ReleaseTicket() = %v", err)
	}
	if err := h.CloseTicket("PROJ-2"); err != nil {
		t.Fatalf("CloseTicket() = %v", err)
	}
	if err := h.CommentTicket("PROJ-3", "breadcrumb"); err != nil {
		t.Fatalf("CommentTicket() = %v", err)
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
	h, _ := hostFor(t, config.Config{}, nil)
	if err := h.CachePrune(); err != nil {
		t.Errorf("CachePrune() with nothing declared = %v, want nil (the rung is skipped)", err)
	}
}

// The declared command is run through a shell — a Consumer writes a command line,
// not an argv — with the checkout as its cwd.
func TestCachePruneRunsTheDeclaredCommand(t *testing.T) {
	project := t.TempDir()
	marker := filepath.Join(project, "pruned")
	h, _ := hostFor(t, config.Config{Host: config.Host{ProjectPath: project}, Project: config.Project{CachePruneCommand: "echo ran > pruned"}}, nil)

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
	h, _ := hostFor(t, config.Config{Project: config.Project{CachePruneCommand: "echo nope >&2; exit 3"}}, nil)

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
	h, _ := hostFor(t, config.Config{}, nil)

	removed, err := h.PruneMergedWorktrees()
	if err == nil {
		t.Fatal("PruneMergedWorktrees() error = nil, want the missing script surfaced")
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0 when the prune never ran", removed)
	}
}
