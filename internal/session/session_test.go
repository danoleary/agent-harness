package session

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// Host-sleep time must NOT count against the cap or the idle window (BEH-608).
// watchReason is fed MONOTONIC elapsed durations, which freeze while the host
// sleeps — so a session that was active right up until the host slept, then slept
// ~2h past its wall-clock cap, has only ~2 min of active time and is not killed.
// The earlier wall-clock behaviour (sleep counted, so the cap tripped on wake)
// burned the whole session cap re-deriving identical work across re-launches; the
// no-indefinite-hang guarantee BEH-386 secured comes from the polling ticker
// resuming after wake, not from charging slept time.
func TestWatchReasonExcludesSleepFromLimits(t *testing.T) {
	cap := 30 * time.Minute
	idle := 20 * time.Minute
	// Host slept ~2h, but the monotonic (active) clock advanced only ~2 min, and the
	// stream was alive right up to the sleep (idle elapsed ~2 min too).
	activeElapsed := 2 * time.Minute
	idleElapsed := 2 * time.Minute

	if reason := watchReason(activeElapsed, idleElapsed, cap, idle); reason != "" {
		t.Errorf("host-sleep time must not trip the cap or idle window, got: %q", reason)
	}
}

type fakeLog struct {
	teed    []string
	events  []string
	records []loopstream.Record
}

func (f *fakeLog) Event(msg string) { f.events = append(f.events, msg) }
func (f *fakeLog) Structured(r loopstream.Record) {
	f.events = append(f.events, r.Message)
	f.records = append(f.records, r)
}
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

// Narrated stream events are mirrored into the global loop.jsonl with their
// structured kind (ADR-0005): a tool_use turn is KindToolUse, a terminal result is
// KindSessionResult — so the viewer counts activity and sees the session end
// without re-parsing the console prose.
func TestPumpStdoutMirrorsStructuredKinds(t *testing.T) {
	log := &fakeLog{}
	pumpStdout(strings.NewReader(lineSystem+"\n"+lineToolUse+"\n"+lineResult), "x.jsonl", false, log, &bytes.Buffer{})

	var kinds []loopstream.Kind
	for _, r := range log.records {
		kinds = append(kinds, r.Kind)
	}
	// system/init narrates nothing; tool_use then result narrate two structured events.
	want := []loopstream.Kind{loopstream.KindToolUse, loopstream.KindSessionResult}
	if len(kinds) != len(want) {
		t.Fatalf("structured kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("structured kind[%d] = %q, want %q", i, kinds[i], want[i])
		}
	}
}

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

// The idle heartbeat must fire when the stream stops producing output (in active,
// monotonic time) for longer than the idle limit, even though the hard cap is
// nowhere near. This is the path that catches a genuinely dead API stream: the cap
// may have hours left, but no bytes have arrived while the host was awake, so the
// container is killed.
func TestWatchReasonFiresIdleWhenStreamSilent(t *testing.T) {
	cap := 2 * time.Hour     // plenty of cap left
	idle := 10 * time.Minute // heartbeat window
	activeElapsed := 15 * time.Minute
	idleElapsed := 11 * time.Minute // 11 min of active silence since last byte

	reason := watchReason(activeElapsed, idleElapsed, cap, idle)
	if reason == "" {
		t.Fatal("expected the idle heartbeat to fire on a silent stream")
	}
	if !strings.Contains(reason, "activity") {
		t.Errorf("expected an idle/activity reason, got: %q", reason)
	}
}

// A genuine awake overrun — the agent kept streaming past the cap, no sleep
// involved — must still trip the cap. Active time exceeds hardCap while the stream
// was live until moments ago, so the cap path fires.
func TestWatchReasonFiresCapOnAwakeOverrun(t *testing.T) {
	cap := 30 * time.Minute
	idle := 20 * time.Minute
	activeElapsed := 31 * time.Minute // 31 min of real, active work
	idleElapsed := 1 * time.Minute    // stream alive a minute ago

	reason := watchReason(activeElapsed, idleElapsed, cap, idle)
	if !strings.Contains(reason, "cap") {
		t.Errorf("expected the cap to fire on a genuine awake overrun, got: %q", reason)
	}
}

// When both limits are breached at the observation tick, the one whose deadline
// came first must be reported (the truthful cause). A session that went idle ~5
// min in and then ran past the cap was dead before the cap; the idle window was
// crossed first (active 5 min + 20 min idle = 25 min < 30 min cap), so the reason
// must name the dead stream, not the cap (the BEH-538 truthfulness property,
// preserved on the active-time axis).
func TestWatchReasonReportsTheLimitCrossedFirst(t *testing.T) {
	cap := 30 * time.Minute
	idle := 20 * time.Minute
	// Last byte at active-minute 5, then silence; observed at active-minute 40 (both
	// limits now past). lastActivityAt = 40 - 35 = 5 min.
	activeElapsed := 40 * time.Minute
	idleElapsed := 35 * time.Minute

	reason := watchReason(activeElapsed, idleElapsed, cap, idle)
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
	// Both within limits → no kill.
	if r := watchReason(5*time.Minute, 5*time.Minute, 30*time.Minute, 10*time.Minute); r != "" {
		t.Errorf("expected no kill with headroom, got: %q", r)
	}

	// cap disabled (<=0): a huge active elapsed must not trip the cap; idle still
	// governs (and here the stream is live, so no kill).
	if r := watchReason(100*time.Hour, 0, 0, 10*time.Minute); r != "" {
		t.Errorf("cap<=0 must disable the cap, got: %q", r)
	}

	// idle disabled (<=0): a long active silence must not trip idle; cap still
	// governs (and here the cap has headroom).
	if r := watchReason(20*time.Minute, 20*time.Minute, 30*time.Minute, 0); r != "" {
		t.Errorf("idle<=0 must disable the heartbeat, got: %q", r)
	}

	// Both breached, idle deadline first (idle from the start, lastActivityAt = 0) →
	// idle reported.
	if r := watchReason(100*time.Hour, 100*time.Hour, 30*time.Minute, 10*time.Minute); !strings.Contains(r, "activity") {
		t.Errorf("expected the idle stall (crossed first) to be reported, got: %q", r)
	}

	// Both breached, cap deadline first (stream alive until active-minute 25, so the
	// idle deadline is minute 35, later than the minute-30 cap) → cap reported.
	if r := watchReason(100*time.Hour, 100*time.Hour-25*time.Minute, 30*time.Minute, 10*time.Minute); !strings.Contains(r, "cap") {
		t.Errorf("expected the cap (crossed first) to be reported, got: %q", r)
	}
}

// watchLimit classifies which limit fired so the watchdog can flag a cap-kill (a
// 137 that must NOT be treated as a bare OOM — BEH-668). capHit is true only when
// the CAP is the fired limit; an idle-window kill, or no kill at all, leaves it
// false. The tie-break mirrors watchReason: when both are crossed, the limit whose
// deadline elapsed first wins.
func TestWatchLimitClassifiesCapVsIdle(t *testing.T) {
	cases := []struct {
		name                                    string
		activeElapsed, idleElapsed, cap, idleTO time.Duration
		wantReason                              string // substring, "" means no kill
		wantCap                                 bool
	}{
		{"cap alone", 30 * time.Minute, 1 * time.Minute, 30 * time.Minute, 10 * time.Minute, "cap", true},
		{"idle alone", 20 * time.Minute, 10 * time.Minute, 30 * time.Minute, 10 * time.Minute, "activity", false},
		{"both, cap deadline first", 100 * time.Hour, 100*time.Hour - 25*time.Minute, 30 * time.Minute, 10 * time.Minute, "cap", true},
		{"both, idle deadline first", 100 * time.Hour, 100 * time.Hour, 30 * time.Minute, 10 * time.Minute, "activity", false},
		{"neither", 5 * time.Minute, 5 * time.Minute, 30 * time.Minute, 10 * time.Minute, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := watchLimit(c.activeElapsed, c.idleElapsed, c.cap, c.idleTO)
			if c.wantReason == "" && v.reason != "" {
				t.Fatalf("expected no kill, got reason %q", v.reason)
			}
			if c.wantReason != "" && !strings.Contains(v.reason, c.wantReason) {
				t.Fatalf("reason %q does not contain %q", v.reason, c.wantReason)
			}
			if v.capHit != c.wantCap {
				t.Errorf("capHit = %v, want %v (reason %q)", v.capHit, c.wantCap, v.reason)
			}
		})
	}
}

// When a kill does land and the host also slept during the session, the kill log
// names the slept time as context — confirming it was excluded, not charged
// (BEH-608). The gap between wall-elapsed and monotonic (active) elapsed at kill
// time is exactly the time slept, so a 30 min active cap reached after 66 min of
// wall time reads as "host also slept ~36m — not counted".
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

// The end-of-session accounting line reports the ACTIVE (monotonic) elapsed the cap
// actually bounds, against the cap — the only number comparable to `cap N min`
// (BEH-688). Without a sleep, active tracks wall, so the line stays a clean
// "active … of … cap" with no sleep note.
func TestSessionSummaryReportsActiveAgainstCap(t *testing.T) {
	s := sessionSummary(28*time.Minute, 28*time.Minute+300*time.Millisecond, 30*time.Minute)
	if !strings.Contains(s, "active 28m") {
		t.Errorf("expected the active elapsed the cap bounds, got: %q", s)
	}
	if !strings.Contains(s, "30m0s cap") {
		t.Errorf("expected the cap named so active and cap are comparable, got: %q", s)
	}
	if strings.Contains(s, "slept") {
		t.Errorf("no host sleep → no sleep note, got: %q", s)
	}
}

// The whole point of the line (BEH-688): claude's own wall-clock `duration_ms`
// reads far above the cap when the host slept mid-session, since host sleep is
// excluded from the cap but not from wall time. A session with only 28m of active
// work but 64m of wall time must report 28m against the 30m cap (NOT blown) and
// name the ~36m of excluded sleep, so the large claude duration is explained rather
// than read as a 2x cap overrun.
func TestSessionSummaryExplainsWallInflationAsExcludedSleep(t *testing.T) {
	s := sessionSummary(28*time.Minute, 64*time.Minute, 30*time.Minute)
	if !strings.Contains(s, "active 28m") {
		t.Errorf("active time (under the cap) must be reported, got: %q", s)
	}
	if !strings.Contains(s, "36m") || !strings.Contains(s, "slept") {
		t.Errorf("expected the ~36m of excluded host sleep named, got: %q", s)
	}
}

// A disabled cap (<=0) has nothing to compare against, so the line reports the bare
// active elapsed with no "cap" clause.
func TestSessionSummaryOmitsCapWhenDisabled(t *testing.T) {
	s := sessionSummary(12*time.Minute, 12*time.Minute, 0)
	if !strings.Contains(s, "active 12m") {
		t.Errorf("expected the active elapsed, got: %q", s)
	}
	if strings.Contains(s, "cap") {
		t.Errorf("a disabled cap must not be named, got: %q", s)
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
