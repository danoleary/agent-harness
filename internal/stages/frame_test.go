package stages

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// The frame is the lifecycle every Stage shares (#29). These tests are its only
// ones: a Stage body test asserts what the body decides, not the frame again.

// records reads the loop.jsonl a Stage's logger feeds — the viewer's stream.
func records(t *testing.T, log *runlog.Logger) []loopstream.Record {
	t.Helper()
	f, err := os.Open(loopstream.PathUnder(filepath.Dir(log.Dir)))
	if err != nil {
		t.Fatalf("open loop.jsonl: %v", err)
	}
	defer f.Close()
	var out []loopstream.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r loopstream.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("decode %q: %v", sc.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

// A Stage announces itself to the viewer once, naming the run and the ticket, and
// says when the run is only a plan.
func TestFrameStartRecordsTheStageStart(t *testing.T) {
	for _, dry := range []bool{false, true} {
		log := stageLog(t, "PROJ-1")
		newFrame(hostio.NewFake(), log, reviewSession, "PROJ-1").start(dry)

		got := records(t, log)
		if len(got) != 1 || got[0].Kind != loopstream.KindStageStart || got[0].Stage != reviewSession || got[0].Ticket != "PROJ-1" {
			t.Fatalf("start records = %+v, want one review stage-start for PROJ-1", got)
		}
		want := "run fakerun — review PROJ-1"
		if dry {
			want += " (dry-run)"
		}
		if got[0].Message != want {
			t.Errorf("start message = %q, want %q", got[0].Message, want)
		}
	}
}

// fetch hands the body the tracker and the ticket, and narrates what it fetched.
// Either half failing is the Stage's hard error.
func TestFrameFetchResolvesTheTicket(t *testing.T) {
	h := hostio.NewFake()
	h.Trk.Ticket = ticket.Ticket{Title: "a ticket", Priority: "High"}
	log := stageLog(t, "PROJ-1")

	client, tk, err := newFrame(h, log, reviewSession, "PROJ-1").fetch()
	if err != nil || client == nil || tk.Identifier != "PROJ-1" {
		t.Fatalf("fetch = %v, %+v, %v; want PROJ-1 from the Host's tracker", client, tk, err)
	}
	if !strings.Contains(narration(t, log), "fetched PROJ-1 (High) — a ticket") {
		t.Errorf("fetch must narrate the ticket; run.jsonl = %s", narration(t, log))
	}

	h.Trk.FetchErr = errors.New("tracker down")
	if _, _, err := newFrame(h, log, reviewSession, "PROJ-1").fetch(); err == nil {
		t.Error("a failed fetch is the Stage's error")
	}
	h.TrackerErr = errors.New("no tracker")
	if _, _, err := newFrame(h, log, reviewSession, "PROJ-1").fetch(); err == nil {
		t.Error("an unresolvable tracker is the Stage's error")
	}
}

// An unset priority reads as such, rather than as an empty pair of brackets.
func TestFrameFetchNamesAMissingPriority(t *testing.T) {
	log := stageLog(t, "PROJ-1")
	if _, _, err := newFrame(hostio.NewFake(), log, reviewSession, "PROJ-1").fetch(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(narration(t, log), "fetched PROJ-1 (No priority) — a ticket") {
		t.Errorf("run.jsonl = %s", narration(t, log))
	}
}

// dropbox creates the findings dir and clears what a prior run of the ticket left,
// so neither a stale out.json re-files nor satisfies this run's ground truth.
func TestFrameDropboxCreatesTheDirAndClearsAStaleDropbox(t *testing.T) {
	log := stageLog(t, "PROJ-1")
	dir := filepath.Join(log.Dir, "findings", "x")
	f := newFrame(hostio.NewFake(), log, implementationSession, "PROJ-1")

	if res, ok := f.dropbox(dir); !ok {
		t.Fatalf("dropbox on a fresh dir = %+v, want ok", res)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.json"), []byte(`[{"title":"stale"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, ok := f.dropbox(dir); !ok {
		t.Fatalf("dropbox over a stale out.json = %+v, want ok", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a prior run's out.json must be cleared, stat err = %v", err)
	}
}

// A findings dir that cannot be created is the Stage's hard error.
func TestFrameDropboxFailsWhenTheDirCannotBeCreated(t *testing.T) {
	log := stageLog(t, "PROJ-1")
	blocker := filepath.Join(log.Dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res, ok := newFrame(hostio.NewFake(), log, implementationSession, "PROJ-1").dropbox(filepath.Join(blocker, "findings"))
	if ok || res.Err == nil {
		t.Errorf("dropbox under a file = %+v, %v; want a hard error", res, ok)
	}
}

// launch announces the sandbox to the viewer with the run's cap, runs it, and
// says where its transcript went — the one place the three Stages narrate a
// session.
func TestFrameLaunchNarratesTheSession(t *testing.T) {
	h := hostio.NewFake()
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{ExitCode: 3} }
	log := stageLog(t, "PROJ-1")

	res := newFrame(h, log, retrospectiveSession, "PROJ-1").launch(hostio.AgentRun{Label: "retrospective", Cap: 20 * time.Minute}, "")
	if res.ExitCode != 3 || len(h.Agents) != 1 {
		t.Fatalf("launch = %+v over %d runs, want the one session's outcome", res, len(h.Agents))
	}
	got := records(t, log)
	if len(got) != 1 || got[0].Kind != loopstream.KindSandboxLaunch || got[0].Stage != retrospectiveSession || got[0].Message != "launching sandbox (cap 20 min active)" {
		t.Errorf("launch records = %+v, want one retrospective sandbox-launch naming the cap", got)
	}
	if want := "retrospective session exited (code 3) — transcript at logs/PROJ-1/" + res.Transcript; !strings.Contains(narration(t, log), want) {
		t.Errorf("launch must name the transcript (%q); run.jsonl = %s", want, narration(t, log))
	}
}

// A Stage that launches more than once names which launch this is.
func TestFrameLaunchCarriesTheLaunchNote(t *testing.T) {
	log := stageLog(t, "PROJ-1")
	newFrame(hostio.NewFake(), log, reviewSession, "PROJ-1").launch(hostio.AgentRun{Label: "review", Cap: 30 * time.Minute}, "attempt 1/2")
	if got := records(t, log); len(got) != 1 || got[0].Message != "launching sandbox (attempt 1/2, cap 30 min active)" {
		t.Errorf("launch records = %+v", got)
	}
}
