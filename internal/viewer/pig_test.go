package viewer

import (
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// Tracer: the pig's state is selected from an event's kind — a tool-use means the
// loop is busy, so the pig is working.
func TestPigStateForToolUseIsWorking(t *testing.T) {
	got := pigStateFor(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})
	if got != pigWorking {
		t.Fatalf("tool-use should select the working state, got %q", got)
	}
}

// The kind→state mapping from the ticket, plus the success/failure split on
// session-result (which carries no structured status — only the ✓/✗ mark in its
// narration, the contract stream.NarrateRecord emits).
func TestPigStateForMapsEachKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  loopstream.Record
		want pigState
	}{
		{"tool-use → working", loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"}, pigWorking},
		{"sandbox-launch → waiting", loopstream.Record{Kind: loopstream.KindSandboxLaunch, Message: "launching sandbox"}, pigWaiting},
		{"idle → sleeping", loopstream.Record{Kind: loopstream.KindIdle, Message: "queue empty"}, pigSleeping},
		{"pr-opened → celebrating", loopstream.Record{Kind: loopstream.KindPROpened, Message: "PR opened"}, pigCelebrating},
		{"breaker-trip → hurt", loopstream.Record{Kind: loopstream.KindBreakerTrip, Message: "winding down"}, pigHurt},
		{"failing session-result → hurt", loopstream.Record{Kind: loopstream.KindSessionResult, Message: "✗ session error (5s)"}, pigHurt},
		{"successful session-result → working", loopstream.Record{Kind: loopstream.KindSessionResult, Message: "✓ session success (5s)"}, pigWorking},
	} {
		if got := pigStateFor(tc.rec); got != tc.want {
			t.Fatalf("%s: pigStateFor = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Exhaustiveness guard, mirroring TestEveryKindIsHandled: every loopstream.Kind
// must select one of the defined pig states, so a Kind added to the closed enum
// can never leave the pig in an undefined mood.
func TestEveryKindSelectsAPigState(t *testing.T) {
	defined := map[pigState]bool{
		pigWorking: true, pigWaiting: true, pigSleeping: true,
		pigCelebrating: true, pigHurt: true,
	}
	for _, k := range loopstream.AllKinds() {
		got := pigStateFor(loopstream.Record{Kind: k})
		if !defined[got] {
			t.Fatalf("kind %q selected undefined pig state %q", k, got)
		}
	}
}

// AC3: the render always carries the state name in text, so the view is legible
// with motion disabled and to a screen reader / log scrape — never animation-only.
func TestRenderPigLabelsStateInText(t *testing.T) {
	for _, st := range []pigState{pigWorking, pigWaiting, pigSleeping, pigCelebrating, pigHurt, pigStopping} {
		out := renderPig(st, 0, true)
		if !strings.Contains(out, string(st)) {
			t.Fatalf("render of %q must contain the state name in text, got:\n%s", st, out)
		}
	}
}

// AC2 (static path): with animation off, the frame index is ignored — every tick
// renders the same single frame for the state.
func TestRenderPigStaticIgnoresFrame(t *testing.T) {
	first := renderPig(pigWorking, 0, false)
	for frame := 1; frame < 6; frame++ {
		if got := renderPig(pigWorking, frame, false); got != first {
			t.Fatalf("static render must not vary with frame %d:\n%q\nvs\n%q", frame, got, first)
		}
	}
}

// AC2 (animating path): frames cycle — advancing the tick changes the rendered
// frame, and the cycle wraps so the index can grow without bound.
func TestRenderPigAnimatesAndWraps(t *testing.T) {
	f0 := renderPig(pigWorking, 0, true)
	f1 := renderPig(pigWorking, 1, true)
	if f0 == f1 {
		t.Fatalf("animating render must advance between frame 0 and 1, both:\n%q", f0)
	}
	if wrapped := renderPig(pigWorking, pigFrameCount(pigWorking), true); wrapped != f0 {
		t.Fatalf("frame index must wrap modulo the cycle length back to frame 0")
	}
}
