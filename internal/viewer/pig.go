package viewer

import (
	"strings"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// pigState is the mascot's mood, selected from the most recent event kind
// (ADR-0005). It is the closed set the dashboard renders the ASCII pig for, and
// every loopstream.Kind maps to one (asserted by TestEveryKindSelectsAPigState),
// so a new kind never leaves the pig in an undefined state.
type pigState string

const (
	pigWorking     pigState = "working"     // busy: a stage is running and tools are firing
	pigWaiting     pigState = "waiting"     // a sandbox is spinning up
	pigSleeping    pigState = "sleeping"    // the queue is empty / nothing claimed
	pigCelebrating pigState = "celebrating" // a PR just opened
	pigHurt        pigState = "hurt"        // a session failed or the breaker tripped
	pigStopping    pigState = "stopping"    // a STOP sentinel is present: winding down
	pigStopped     pigState = "stopped"     // the loop has wound down and exited (BEH-613)
)

// pigStateFor selects the pig's state from a record. The five states the ticket
// pins (tool-use→working, sandbox-launch→waiting, idle→sleeping,
// pr-opened→celebrating, failing session-result / breaker-trip→hurt) are exact;
// the remaining kinds fall to the nearest mood: an active step is working, a
// released/abandoned ticket is sleeping, and an abnormal abort (cap-abort) is hurt
// — the same family as breaker-trip.
func pigStateFor(r loopstream.Record) pigState {
	switch r.Kind {
	case loopstream.KindSandboxLaunch:
		return pigWaiting
	case loopstream.KindPROpened:
		return pigCelebrating
	case loopstream.KindLoopStopped:
		// The terminal record: the daemon has wound down and exited. A distinct,
		// calm "stopped" mascot reads as done — not the breaker's "hurt" nor the
		// still-alive "stopping" wave — so a clean stop is unmistakable (BEH-613).
		return pigStopped
	case loopstream.KindBreakerTrip, loopstream.KindCapAbort:
		return pigHurt
	case loopstream.KindIdle, loopstream.KindTicketReleased, loopstream.KindCapBackoff:
		// Backing off after a cap abort is a waiting-it-out state, like an empty queue:
		// the daemon is alive and counting down, not working — so the pig sleeps.
		return pigSleeping
	case loopstream.KindSessionResult:
		if sessionResultFailed(r.Message) {
			return pigHurt
		}
		return pigWorking
	default:
		// ticket-selected, stage-start, tool-use, and any successful terminal state:
		// the loop is making progress, so the pig is working.
		return pigWorking
	}
}

// pigFrames is the hand-rolled, stdlib-only animation: a short loop of pure-ASCII
// frames per state, advanced on the dashboard's redraw ticker (ADR-0005 — no TUI
// dependency). Each state has ≥2 frames so it visibly cycles; the difference between
// frames is the motion — trotting feet, blinking eyes, floating Zzz, a party bounce,
// a wince, or waving goodbye while walking off. The art is four lines tall with a
// snout and legs so the mood reads at a glance; the leading spaces keep it clear of
// the dashboard's panel labels.
var pigFrames = map[pigState][]string{
	pigWorking: {
		"    ,----.\n   ( o  o )\n   ( =oo= )\n    /'  '\\",
		"    ,----.\n   ( o  o )\n   ( =oo= )\n    \\,  ,/",
	},
	pigWaiting: {
		"    ,----.\n   ( -  - )\n   ( =oo= )  ...\n    /    \\",
		"    ,----.\n   ( o  o )\n   ( =oo= )  :::\n    /    \\",
	},
	pigSleeping: {
		"    ,----.   z\n   ( -  - )  Z\n   ( =oo= )  z\n    /    \\",
		"    ,----.   Z\n   ( -  - )  z\n   ( =oo= )  Z\n    /    \\",
	},
	pigCelebrating: {
		"   \\,----./\n   ( ^  ^ )  *\n   ( =oo= )  !\n    /    \\",
		"   /,----.\\\n   ( ^  ^ ) *\n   ( =oo= ) !\n    /    \\",
	},
	pigHurt: {
		"    ,----.\n   ( x  x )\n   ( =--= )\n    /    \\",
		"    ,----.\n   ( X  X )\n   ( =--= )\n    /    \\",
	},
	pigStopping: {
		"    ,----.   bye~\n   ( o  o )  /\n   ( =oo= )\n    >    >",
		"    ,----.   bye~\n   ( o  o )  \\\n   ( =oo= )\n    >>   >>",
	},
	pigStopped: {
		"    ,----.   .\n   ( -  - )  [x]\n   ( =--= )\n    |    |",
		"    ,----.\n   ( -  - )  [x]\n   ( =--= )\n    |    |",
	},
}

// pigFrameCount is the number of frames in a state's cycle (≥1).
func pigFrameCount(s pigState) int { return len(pigFrames[s]) }

// renderPig renders the pig for a state as ASCII art plus a text label of the
// state (AC3 — never animation-only). When animate is false the frame index is
// ignored and the single first frame is rendered (the reduced-motion / static
// path, AC2); when true the frame index selects a frame, wrapping modulo the
// cycle length so an unbounded tick counter stays in range.
func renderPig(s pigState, frame int, animate bool) string {
	frames := pigFrames[s]
	idx := 0
	if animate && len(frames) > 0 {
		idx = ((frame % len(frames)) + len(frames)) % len(frames)
	}
	art := ""
	if idx < len(frames) {
		art = frames[idx]
	}
	return art + "\n  status: " + string(s)
}

// sessionResultFailed reports whether a session-result narration is a failure. The
// record carries no structured status field — only the human Message — so the
// signal is the leading ✗ mark stream.NarrateRecord emits for an is_error result
// (vs ✓ for success). That mark is a stable narration contract (BEH-495 coerces
// the contradictory "✗ … success" shape so the mark always agrees with the word).
func sessionResultFailed(message string) bool {
	return strings.HasPrefix(strings.TrimSpace(message), "✗")
}
