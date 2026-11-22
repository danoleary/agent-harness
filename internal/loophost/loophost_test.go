package loophost

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/hostlock"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/prloop"
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
	h.lookPath = func(string) (string, error) { return "/usr/bin/gh", nil }
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

// #383: every PR the daemon opens, reads or watches goes through gh, so a daemon
// on a host whose PATH has no gh refuses to start, naming the fix, before the token
// or the tracker is read — instead of losing each ticket's CI verdict to it later.
func TestCheckAccessRefusesToStartWithoutGh(t *testing.T) {
	var cfg config.Config
	h := NewDaemon(withProject(t, cfg), config.Loop{Name: "tickets"}, nil)
	missing := &exec.Error{Name: "gh", Err: exec.ErrNotFound}
	h.lookPath = func(string) (string, error) { return "", missing }
	tokenRead := false
	h.checkToken = func() error { tokenRead = true; return nil }

	err := h.CheckAccess()

	if !errors.Is(err, exec.ErrNotFound) || !strings.Contains(err.Error(), "install it from https://cli.github.com") {
		t.Errorf("CheckAccess() = %v, want the missing gh and how to install it", err)
	}
	if tokenRead {
		t.Error("the token must not be read once gh is known to be missing")
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

// The sandbox mounts the checkout read-write, so a session can rewrite the prune
// script the host runs between tickets. The host runs the copy on origin/main,
// which only a merged PR changes (#332).
func TestPruneMergedWorktreesRunsTheScriptOnOriginMain(t *testing.T) {
	repo := gittest.New(t)
	script := filepath.Join(repo.Dir, "scripts", "prune-merged-worktrees.sh")
	write := func(body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("#!/bin/sh\necho 'Pruned 2 worktree(s).'\n")
	repo.Git(repo.Dir, "add", "-A")
	repo.Git(repo.Dir, "commit", "-q", "-m", "add the prune script")
	repo.Git(repo.Dir, "push", "-q", "origin", "main")
	ran := filepath.Join(t.TempDir(), "ran")
	write("#!/bin/sh\ntouch '" + ran + "'\necho 'Pruned 9 worktree(s).'\n")

	var cfg config.Config
	cfg.ProjectPath = repo.Dir
	removed, err := hostFor(t, cfg).PruneMergedWorktrees()
	if err != nil {
		t.Fatalf("PruneMergedWorktrees: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want origin/main's script's 2", removed)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the working tree's prune script ran on the host")
	}
}

// origin/main's prune script is still run by a git that reads the checkout's
// .git/config, which every sandbox can write. A core.hooksPath the session set
// there would have the script's own `git checkout` run a hook on the host, next to
// GH_TOKEN, so the script runs with the host-side overrides in its environment
// (#350, ADR-0057).
func TestPruneMergedWorktreesScriptGitRunsNoHookTheSandboxPlanted(t *testing.T) {
	repo := gittest.New(t)
	script := filepath.Join(repo.Dir, "scripts", "prune-merged-worktrees.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nset -e\ngit checkout -q -b probe\necho 'Pruned 1 worktree(s).'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "add", "-A")
	repo.Git(repo.Dir, "commit", "-q", "-m", "add the prune script")
	repo.Git(repo.Dir, "push", "-q", "origin", "main")

	ran := filepath.Join(t.TempDir(), "ran")
	hooks := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "config", "core.hooksPath", hooks)

	var cfg config.Config
	cfg.ProjectPath = repo.Dir
	if _, err := hostFor(t, cfg).PruneMergedWorktrees(); err != nil {
		t.Fatalf("PruneMergedWorktrees: %v", err)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the prune script's git ran a hook the sandbox planted, on the host")
	}
}

// #404: a ticket's retrospective runs after its PR is open, so once the PR merges
// its worktree is merged and clean, the Consumer's prune script's whole test for
// "dead". Another loop's disk reclaim ran that script a minute into the session
// and removed the worktree from under it. Replayed: #404's run holds its run lock
// while a prune that removes every merged, clean worktree under
// .claude/worktrees runs (one that forces the removal, as a script may). The live
// run's worktree stays, and is not left locked once the prune is done; a worktree
// no run is in is reclaimed as before.
func TestPruneMergedWorktreesLeavesAWorktreeALiveRunIsIn(t *testing.T) {
	repo := gittest.New(t)
	script := filepath.Join(repo.Dir, "scripts", "prune-merged-worktrees.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nn=0\nfor wt in $(git worktree list --porcelain | sed -n 's|^worktree \\(.*/.claude/worktrees/.*\\)|\\1|p'); do\n" +
		"  if git worktree remove --force \"$wt\" 2>/dev/null; then n=$((n+1)); fi\ndone\necho \"Pruned $n worktree(s).\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "add", "-A")
	repo.Git(repo.Dir, "commit", "-q", "-m", "add the prune script")
	repo.Git(repo.Dir, "push", "-q", "origin", "main")
	live, dead := repo.Worktree("gh-404"), repo.Worktree("gh-399")
	for _, wt := range []gitpkg.Worktree{live, dead} {
		if err := wt.Create(); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = repo.Dir
	release, err := HoldRun(cfg, "#404", "run-retro", &narrator{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	removed, err := hostFor(t, cfg).PruneMergedWorktrees()
	if err != nil {
		t.Fatalf("PruneMergedWorktrees: %v", err)
	}
	if !live.Exists() {
		t.Fatal("the prune removed #404's worktree while its run, in its retrospective, held the run lock")
	}
	if dead.Exists() || removed != 1 {
		t.Errorf("removed = %d, worktree no run is in still there: %v; want it reclaimed", removed, dead.Exists())
	}
	if out := repo.Git(repo.Dir, "worktree", "list", "--porcelain"); strings.Contains(out, "locked") {
		t.Errorf("the prune left a worktree locked:\n%s", out)
	}
}

// #416: a PR-loop run rebases the PR's branch, pushes it, and then runs its
// retrospective in its own worktree, .claude/worktrees/pr-<n>. Once the pushed PR
// merges, that worktree is merged and clean, and the run held no run lock, so the
// shield of #404 did not see it: another loop's prune removed it from under the
// session. Replayed: the PR loop's run holds PR-416's run lock through
// [RunLock] while a prune that removes every merged, clean worktree runs. The
// run's worktree stays, and the lock is let go when the run ends.
func TestPruneMergedWorktreesLeavesAWorktreeAPRLoopRunIsIn(t *testing.T) {
	repo := gittest.New(t)
	script := filepath.Join(repo.Dir, "scripts", "prune-merged-worktrees.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\nn=0\nfor wt in $(git worktree list --porcelain | sed -n 's|^worktree \\(.*/.claude/worktrees/.*\\)|\\1|p'); do\n" +
		"  if git worktree remove --force \"$wt\" 2>/dev/null; then n=$((n+1)); fi\ndone\necho \"Pruned $n worktree(s).\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "add", "-A")
	repo.Git(repo.Dir, "commit", "-q", "-m", "add the prune script")
	repo.Git(repo.Dir, "push", "-q", "origin", "main")
	wt := repo.Worktree(ticket.Slug(prloop.Key(416)))
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}}
	cfg.ProjectPath = repo.Dir
	lock := hostlock.File(config.RunLockFile(cfg.ProjectPath, ticket.Slug(prloop.Key(416))))
	got := RunLock(cfg, &narrator{})(prloop.Key(416), "run-retro", func() stages.Result {
		if _, err := hostFor(t, cfg).PruneMergedWorktrees(); err != nil {
			t.Fatalf("PruneMergedWorktrees: %v", err)
		}
		return stages.Result{OK: true, Disposition: stages.Shipped}
	})
	if !wt.Exists() {
		t.Fatal("the prune removed PR-416's worktree while its PR-loop run, in its retrospective, was live")
	}
	if got.Disposition != stages.Shipped {
		t.Errorf("Disposition = %v, want the run's own", got.Disposition)
	}
	if held, _ := lock.Held(); held {
		t.Error("the run lock is still held after the PR-loop run ended")
	}
}

// A daemon killed mid-prune leaves its shield's lock on a worktree. The next prune
// lifts it once no run is in the worktree, so the worktree is reclaimed and the
// ticket's own reap is not refused for ever; a lock someone else took stays.
func TestPruneMergedWorktreesLiftsAKilledDaemonsShield(t *testing.T) {
	repo := gittest.New(t)
	script := filepath.Join(repo.Dir, "scripts", "prune-merged-worktrees.sh")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'Pruned 0 worktree(s).'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "add", "-A")
	repo.Git(repo.Dir, "commit", "-q", "-m", "add the prune script")
	repo.Git(repo.Dir, "push", "-q", "origin", "main")
	stale, human := repo.Worktree("gh-404"), repo.Worktree("gh-405")
	for _, wt := range []gitpkg.Worktree{stale, human} {
		if err := wt.Create(); err != nil {
			t.Fatal(err)
		}
	}
	repo.Git(repo.Dir, "worktree", "lock", "--reason", liveRunLockReason, stale.Path())
	repo.Git(repo.Dir, "worktree", "lock", "--reason", "mine", human.Path())

	var cfg config.Config
	cfg.ProjectPath = repo.Dir
	if _, err := hostFor(t, cfg).PruneMergedWorktrees(); err != nil {
		t.Fatalf("PruneMergedWorktrees: %v", err)
	}
	list, err := repo.Checkout().ListWorktrees()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		switch filepath.Base(w.Path) {
		case "gh-404":
			if w.Locked {
				t.Errorf("the killed daemon's shield on %s is still there", w.Path)
			}
		case "gh-405":
			if !w.Locked || w.LockReason != "mine" {
				t.Errorf("%s = %+v, want the lock someone else took left as it was", w.Path, w)
			}
		}
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
		release, err := HoldRun(h.cfg, key, "run-"+key, &narrator{})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
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

	release, err := HoldRun(src.cfg, "BEH-1301", "run-a", &narrator{})
	if err != nil {
		t.Fatal(err)
	}
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

// #384: the PR loop reads a ticket worktree's own run lock before it reclaims the
// worktree, so a live run whose claim the tracker's claim list misses still keeps
// it. The worktree is live while its ticket's run holds the lock, and a leftover
// again once the run ends.
func TestAWorktreeIsLiveWhileItsTicketsRunHoldsTheRunLock(t *testing.T) {
	cfg := withProject(t, config.Config{Project: config.Project{BranchPrefix: gittest.Prefix}})
	wts := NewWorktrees(cfg)
	path := gitpkg.Open(cfg.ProjectPath, cfg.BranchPrefix).Worktree("beh-1301").Path()

	release, err := HoldRun(cfg, "BEH-1301", "run-a", &narrator{})
	if err != nil {
		t.Fatal(err)
	}
	if live, err := wts.RunLive(path); err != nil || !live {
		t.Fatalf("RunLive(%s) = %v, %v while BEH-1301's run holds its lock, want true", path, live, err)
	}
	release()
	if live, err := wts.RunLive(path); err != nil || live {
		t.Errorf("RunLive(%s) = %v, %v after the run ended, want false", path, live, err)
	}
	if live, err := wts.RunLive("/Users/someone/herd-trim-radix"); err != nil || live {
		t.Errorf("RunLive of a worktree no run ever locked = %v, %v, want false", live, err)
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
	release, err := HoldRun(cfg, "BEH-2", "run-a", log)
	if err != nil {
		t.Fatalf("HoldRun() error = %v, want the lock taken once the probe let go", err)
	}
	defer release()
	if len(log.events) != 0 {
		t.Errorf("events = %v, want the run lock taken once the probe let go", log.events)
	}
	if held, _ := hostlock.File(config.RunLockFile(cfg.ProjectPath, "beh-2")).Held(); !held {
		t.Error("the run must hold its lock after a probe let go")
	}
}

// #366: a second run of a ticket whose run lock another live run holds launches
// no session — no Stage runs at all. It says why, comes back RunLive for the loop
// to leave the claim to the live run, and leaves that run's lock held.
func TestASecondRunOfATicketWhoseRunIsLiveRunsNothing(t *testing.T) {
	cfg := withProject(t, config.Config{})
	first, err := HoldRun(cfg, "BEH-1470", "run-a", &narrator{})
	if err != nil {
		t.Fatal(err)
	}
	defer first()

	log := &narrator{}
	ran := false
	got := underRunLock(cfg, "BEH-1470", "run-b", log, func() stages.Result { ran = true; return stages.Result{OK: true} })
	if ran {
		t.Fatal("the second run launched its Stages alongside the live one")
	}
	if got.Disposition != stages.RunLive {
		t.Errorf("Disposition = %v, want %v", got.Disposition, stages.RunLive)
	}
	if len(log.events) != 1 || !strings.Contains(log.events[0], "another live run holds BEH-1470's run lock") {
		t.Errorf("events = %v, want one line naming BEH-1470 and why", log.events)
	}
	if owner, held, _ := hostlock.File(config.RunLockFile(cfg.ProjectPath, "beh-1470")).Holder(); !held || owner != "run-a" {
		t.Errorf("run lock holder = %q (held %v), want run-a's lock untouched", owner, held)
	}
	if _, err := HoldRun(cfg, "BEH-1470", "run-c", &narrator{}); !errors.Is(err, ErrRunLive) {
		t.Errorf("HoldRun() error = %v, want ErrRunLive", err)
	}
}

// A run whose lock is free runs under it, and lets it go when it ends.
func TestARunHoldsItsRunLockWhileItRuns(t *testing.T) {
	cfg := withProject(t, config.Config{})
	lock := hostlock.File(config.RunLockFile(cfg.ProjectPath, "gh-366"))
	got := underRunLock(cfg, "#366", "run-a", &narrator{}, func() stages.Result {
		if owner, held, _ := lock.Holder(); !held || owner != "run-a" {
			t.Errorf("while the run runs, holder = %q (held %v), want run-a", owner, held)
		}
		return stages.Result{OK: true, Disposition: stages.Shipped}
	})
	if got.Disposition != stages.Shipped {
		t.Errorf("Disposition = %v, want the run's own", got.Disposition)
	}
	if held, _ := lock.Held(); held {
		t.Error("the run lock is still held after the run ended")
	}
}

// #366: the reaper reads a claim's age by the wall clock, which a host sleep
// advances under a live run. The Source says which claims a live run holds, so
// the reaper leaves those.
func TestClaimsSayWhichRunsAreLive(t *testing.T) {
	trk := newQueueTracker()
	trk.claims = []tracker.InProgressClaim{{Identifier: "BEH-1470"}, {Identifier: "BEH-1483"}}
	src := sourceFor(t, config.Config{}, trk)
	release, err := HoldRun(src.cfg, "BEH-1470", "run-a", &narrator{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	claims, err := src.Claims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || !claims[0].Live || claims[1].Live {
		t.Errorf("claims = %+v, want BEH-1470 live and BEH-1483 not", claims)
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
