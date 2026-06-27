package session

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The wall-clock cap must count time the host spent asleep. We model a sleep as a
// large forward jump in the wall clock between start and the watchdog tick (Go's
// monotonic timers freeze during macOS sleep, so the old time.AfterFunc cap never
// fired for a container that lost its API stream mid-sleep — the bug this fixes).
// watchReason reads wall-clock times only, so the jump counts and the cap trips.
func TestWatchReasonFiresCapAcrossSleepJump(t *testing.T) {
	start := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	cap := 30 * time.Minute
	// Only ~2 min of awake time elapsed, then the host slept ~2h: wall clock is now
	// well past the cap even though a monotonic timer would have advanced ~2 min.
	now := start.Add(2*time.Hour + 2*time.Minute)
	lastActivity := now // stream still "live" — isolate the cap path

	reason := watchReason(now, start, lastActivity, cap, 0)
	if reason == "" {
		t.Fatal("expected the wall-clock cap to fire after a sleep jump past it")
	}
	if !strings.Contains(reason, "cap") {
		t.Errorf("expected a cap reason, got: %q", reason)
	}
}

type fakeLog struct {
	teed   []string
	events []string
}

func (f *fakeLog) Event(msg string)      { f.events = append(f.events, msg) }
func (f *fakeLog) TeeLine(_, raw string) { f.teed = append(f.teed, strings.TrimSuffix(raw, "\n")) }

const (
	lineSystem  = `{"type":"system","subtype":"init"}`
	lineToolUse = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"}]}}`
	lineResult  = `{"type":"result","subtype":"success","duration_ms":1000}`
	lineRefusal = `{"type":"result","subtype":"success","is_error":true,"result":"API Error: Claude Code is unable to respond to this request, which appears to violate our Usage Policy. If you are seeing this refusal repeatedly, try running /model to switch models."}`
	lineCap     = `{"type":"result","subtype":"success","is_error":true,"result":"Spending cap reached resets 8:20am"}`
	lineVerdict = `{"type":"assistant","message":{"content":[{"type":"text","text":"## Review: feat/x  (BEH-1 — intent)   2 files, +5/-1"}]}}`
	lineBlocked = `{"type":"assistant","message":{"content":[{"type":"text","text":"## Review: feat/x  (BEH-1 — intent)   2 files, +5/-1\n\nDisposition: blocked — needs a human call"}]}}`
)

// pumpStdout reports whether the review session emitted its seven-lens verdict
// (the "## Review:" report header — BEH-525), so Run can tell a completed review
// from one cut short before the report (e.g. an OOM mid-gate). A stream that never
// reaches the report reports false even though it ends on a benign result.
func TestPumpStdoutReportsReviewVerdict(t *testing.T) {
	reviewed := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineVerdict+"\n"+lineResult), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if !reviewed.reviewVerdictEmitted {
		t.Error("expected the review verdict to be reported when the report header appears")
	}

	clean := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineResult), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if clean.reviewVerdictEmitted {
		t.Error("a stream that never emitted the report header must not report a verdict")
	}
}

// pumpStdout reports whether the review verdict declared a blocked disposition
// (BEH-580) — an unresolved Blocker/Important finding the autonomous reviewer could
// not resolve — so Run can fail the push closed instead of opening a PR with the
// finding unaddressed. A clear verdict reports the verdict but not the block.
func TestPumpStdoutReportsBlockedReviewVerdict(t *testing.T) {
	blocked := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineBlocked+"\n"+lineResult), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if !blocked.reviewBlocked {
		t.Error("expected a blocked disposition to be reported when the verdict declares it")
	}
	if !blocked.reviewVerdictEmitted {
		t.Error("a blocked verdict is still a verdict — reviewVerdictEmitted must also be set")
	}

	clear := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineVerdict+"\n"+lineResult), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if clear.reviewBlocked {
		t.Error("a clear verdict (no blocked disposition) must not report a block")
	}
}

// pumpStdout reports whether the stream carried a terminal usage-policy refusal
// (BEH-389) so Run can flag the session as retryable. A stream without one
// reports false even though it ends on a (benign) result.
func TestPumpStdoutReportsUsagePolicyRefusal(t *testing.T) {
	log := &fakeLog{}
	var echo bytes.Buffer

	refused := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineRefusal), "x.jsonl", false, log, &echo)
	if !refused.usagePolicyRefusal {
		t.Error("expected a usage-policy refusal to be reported")
	}

	clean := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineResult), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if clean.usagePolicyRefusal {
		t.Error("a clean run must not report a usage-policy refusal")
	}
}

// pumpStdout reports whether the stream carried a terminal spending-cap abort
// (BEH-494) so Run can flag it as a distinct retry-after-reset class. A clean run
// (and a usage-policy refusal) reports no cap abort.
func TestPumpStdoutReportsSpendingCapAbort(t *testing.T) {
	capped := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineCap), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if !capped.spendingCapAbort {
		t.Error("expected a spending-cap abort to be reported")
	}
	if capped.usagePolicyRefusal {
		t.Error("a spending-cap abort must not be reported as a usage-policy refusal")
	}

	clean := pumpStdout(strings.NewReader(lineToolUse+"\n"+lineResult), "x.jsonl", false, &fakeLog{}, &bytes.Buffer{})
	if clean.spendingCapAbort {
		t.Error("a clean run must not report a spending-cap abort")
	}
}

// The idle heartbeat must fire when the stream stops producing output for longer
// than the idle limit, even though the hard cap is nowhere near. This is the path
// that catches a dead API stream after the host wakes from sleep: the cap may
// have hours left, but no bytes have arrived, so the container is killed.
func TestWatchReasonFiresIdleWhenStreamSilent(t *testing.T) {
	start := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	cap := 2 * time.Hour     // plenty of cap left
	idle := 10 * time.Minute // heartbeat window
	lastActivity := start.Add(1 * time.Minute)
	now := lastActivity.Add(11 * time.Minute) // 11 min since last byte

	reason := watchReason(now, start, lastActivity, cap, idle)
	if reason == "" {
		t.Fatal("expected the idle heartbeat to fire on a silent stream")
	}
	if !strings.Contains(reason, "activity") {
		t.Errorf("expected an idle/activity reason, got: %q", reason)
	}
}

// A stalled session that the host then sleeps past the hard cap must be reported
// as the stall it was, not as a "late" cap. This is the BEH-538 incident: the
// session went idle ~5 min in, the host slept, and the watchdog tick only landed
// at ~66 min wall-clock — past the 30 min cap. The cap is wall-clock-correct, but
// blaming the cap hides that the session was actually dead 40 min earlier. The
// idle window was crossed first (start+5+20 = 25 min < start+30 = cap), so the
// reason must name the dead stream, not the cap.
func TestWatchReasonReportsTheLimitCrossedFirst(t *testing.T) {
	start := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)
	cap := 30 * time.Minute
	idle := 20 * time.Minute
	lastActivity := start.Add(5 * time.Minute) // last byte ~5 min in, then silence
	now := start.Add(66 * time.Minute)         // watchdog tick after a host sleep

	reason := watchReason(now, start, lastActivity, cap, idle)
	if !strings.Contains(reason, "activity") {
		t.Errorf("a session that went idle before the cap must be reported as a dead stream, got: %q", reason)
	}
	if strings.Contains(reason, "cap") {
		t.Errorf("must not blame the cap for a stall the idle window caught first, got: %q", reason)
	}
}

// The watchdog contract: no kill when both limits have headroom; each limit is
// disabled by a non-positive value (so a caller can opt out of either); and when
// both are breached the one whose deadline came first is reported (the truthful
// cause — see TestWatchReasonReportsTheLimitCrossedFirst).
func TestWatchReasonContract(t *testing.T) {
	start := time.Date(2026, 6, 21, 15, 0, 0, 0, time.UTC)

	// Both within limits → no kill.
	now := start.Add(5 * time.Minute)
	if r := watchReason(now, start, now, 30*time.Minute, 10*time.Minute); r != "" {
		t.Errorf("expected no kill with headroom, got: %q", r)
	}

	// cap disabled (<=0): a huge elapsed must not trip the cap; idle still governs.
	farPast := start.Add(100 * time.Hour)
	if r := watchReason(farPast, start, farPast, 0, 10*time.Minute); r != "" {
		t.Errorf("cap<=0 must disable the cap, got: %q", r)
	}

	// idle disabled (<=0): a long silence must not trip idle; cap still governs.
	silent := start.Add(20 * time.Minute)
	if r := watchReason(silent, start, start, 30*time.Minute, 0); r != "" {
		t.Errorf("idle<=0 must disable the heartbeat, got: %q", r)
	}

	// Both breached, idle deadline first (idle from the start) → idle reported.
	if r := watchReason(farPast, start, start, 30*time.Minute, 10*time.Minute); !strings.Contains(r, "activity") {
		t.Errorf("expected the idle stall (crossed first) to be reported, got: %q", r)
	}

	// Both breached, cap deadline first (stream alive until just before the cap,
	// then the host slept past it) → cap reported. lastActivity = start+25 makes the
	// idle deadline start+35, later than the start+30 cap.
	lateIdle := start.Add(25 * time.Minute)
	if r := watchReason(farPast, start, lateIdle, 30*time.Minute, 10*time.Minute); !strings.Contains(r, "cap") {
		t.Errorf("expected the cap (crossed first) to be reported, got: %q", r)
	}
}

// A wall-clock kill that lands long after its deadline is explained by host sleep:
// the watchdog's poll ticker is monotonic and freezes while the host sleeps, so a
// large gap between wall-elapsed and monotonic-elapsed at kill time is exactly the
// time slept. The kill log must surface it so a 30 min cap observed at 66 min reads
// as "host slept ~36m", not a broken timer (BEH-538).
func TestSleepNoteSurfacesHostSleep(t *testing.T) {
	note := sleepNote(66*time.Minute, 30*time.Minute)
	if !strings.Contains(note, "slept") {
		t.Errorf("expected the note to name host sleep, got: %q", note)
	}
	if !strings.Contains(note, "36m") {
		t.Errorf("expected the note to quantify ~36m of sleep, got: %q", note)
	}
}

// With no sleep, wall and monotonic elapsed track within scheduling jitter, so the
// kill log carries no sleep note — the common case (a genuine overrun or a stall
// while the host was awake) stays clean.
func TestSleepNoteEmptyWhenWallTracksMonotonic(t *testing.T) {
	if note := sleepNote(30*time.Minute+200*time.Millisecond, 30*time.Minute); note != "" {
		t.Errorf("expected no sleep note when wall tracks monotonic, got: %q", note)
	}
}

// heartbeatReader is the liveness tap on the docker stdout stream: every read
// that returns bytes counts as a heartbeat, and a read that returns no bytes
// (EOF / empty) must not — so a dead stream (which only ever returns 0/EOF or
// blocks) produces no heartbeats and the idle watchdog can kill it.
func TestHeartbeatReaderBeatsOnDataNotOnEmptyRead(t *testing.T) {
	var beats int
	hr := heartbeatReader{r: strings.NewReader("ab"), beat: func() { beats++ }}

	buf := make([]byte, 1)
	for {
		if _, err := hr.Read(buf); err != nil {
			break // io.EOF on the read past "ab"
		}
	}

	if beats != 2 {
		t.Errorf("expected one beat per non-empty read (2), got %d", beats)
	}
}

// Non-verbose: every stdout line is teed raw, and narratable lines surface as
// concise events (the default console view).
func TestPumpStdoutTeesAllAndNarrates(t *testing.T) {
	in := strings.Join([]string{lineSystem, lineToolUse, lineResult}, "\n")
	log := &fakeLog{}
	var echo bytes.Buffer

	pumpStdout(strings.NewReader(in), "implementation-x.jsonl", false, log, &echo)

	if len(log.teed) != 3 {
		t.Fatalf("expected all 3 lines teed, got %d: %v", len(log.teed), log.teed)
	}
	joined := strings.Join(log.events, "\n")
	if !strings.Contains(joined, "⚒ Edit") {
		t.Errorf("expected tool_use narration, got: %q", joined)
	}
	if !strings.Contains(joined, "session success") {
		t.Errorf("expected result narration, got: %q", joined)
	}
	if echo.Len() != 0 {
		t.Errorf("non-verbose must not echo raw, got: %q", echo.String())
	}
}

// Verbose: every line is echoed raw to the console and still teed; the concise
// narration is suppressed (the raw stream is the whole point of --verbose).
func TestPumpStdoutVerboseEchoesRawAndSkipsNarration(t *testing.T) {
	in := strings.Join([]string{lineToolUse, lineResult}, "\n")
	log := &fakeLog{}
	var echo bytes.Buffer

	pumpStdout(strings.NewReader(in), "implementation-x.jsonl", true, log, &echo)

	if len(log.teed) != 2 {
		t.Fatalf("expected both lines teed, got %d", len(log.teed))
	}
	if !strings.Contains(echo.String(), lineToolUse) || !strings.Contains(echo.String(), lineResult) {
		t.Errorf("verbose should echo raw lines, got: %q", echo.String())
	}
	if len(log.events) != 0 {
		t.Errorf("verbose must not narrate, got: %v", log.events)
	}
}

// stderr is forensic: every line is teed, and a bounded tail of the last
// non-empty lines is returned so a docker launch failure can be explained.
func TestPumpStderrTeesAllAndReturnsBoundedTail(t *testing.T) {
	var lines []string
	for i := 0; i < 15; i++ {
		lines = append(lines, "err line")
	}
	// Blank lines are teed but excluded from the tail.
	in := strings.Join(lines, "\n") + "\n\n" + "docker: final cause"
	log := &fakeLog{}

	tail := pumpStderr(strings.NewReader(in), "implementation-x.jsonl", log, 10)

	if len(log.teed) != 17 {
		t.Errorf("expected all 17 lines teed (incl. blank), got %d", len(log.teed))
	}
	if len(tail) != 10 {
		t.Fatalf("expected tail bounded to 10, got %d", len(tail))
	}
	if tail[len(tail)-1] != "docker: final cause" {
		t.Errorf("tail should end at the last non-empty line, got: %q", tail[len(tail)-1])
	}
}
