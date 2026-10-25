package stages

import (
	"strings"
	"syscall"
	"testing"

	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/session"
)

// killedBySIGTERM is a session a SIGTERM killed from outside (exit 143) a few
// minutes in, after it had started editing (#99).
func killedBySIGTERM(dirty func()) func(int, hostio.AgentRun) session.Outcome {
	return func(int, hostio.AgentRun) session.Outcome {
		if dirty != nil {
			dirty()
		}
		return session.Outcome{ExitCode: 128 + int(syscall.SIGTERM)}
	}
}

// #99: a signal-killed implementation session was not a failed one. The stage
// reports Interrupted without running the handoff verification — no "no handoff
// commit" verdict, no checkpoint over the half-done tree, no release: the claim
// and the worktree stay for the next run to pick up.
func TestImplementationReportsASignalKilledSessionAsInterrupted(t *testing.T) {
	h, repo := fresh(t, "PROJ-1")
	h.AgentFn = killedBySIGTERM(func() { repo.Dirty(h.Worktree()) })
	log := stageLog(t, "PROJ-1")

	res := Implementation(h, stageCfg(), log, Args{Identifier: "PROJ-1"})

	if res.OK || res.Disposition != Interrupted || res.Signal != syscall.SIGTERM || res.Retryable {
		t.Fatalf("a SIGTERMed session is Interrupted by SIGTERM and not retryable, got %+v", res)
	}
	got := narration(t, log)
	if !strings.Contains(got, "interrupted by signal 15") {
		t.Errorf("the interruption must be named; run.jsonl = %s", got)
	}
	if strings.Contains(got, "tdd ✗") || strings.Contains(got, "checkpoint") {
		t.Errorf("an interrupted session gets no verdict and no checkpoint; run.jsonl = %s", got)
	}
	if h.Worktree().Clean() {
		t.Error("the half-done tree must be left as the session left it, for the next run")
	}
	if len(h.Trk.Released) != 0 {
		t.Errorf("released = %v, want the claim left for the next run", h.Trk.Released)
	}
	if len(h.Agents) != 1 {
		t.Errorf("agent runs = %v, want one — an interruption is not retried in-stage", labels(h.Agents))
	}
}

// #99: a signal-killed review session ends the review there — no host gate re-run,
// no push — and reports Interrupted.
func TestReviewReportsASignalKilledSessionAsInterrupted(t *testing.T) {
	h, repo := handedOff(t, "PROJ-2")
	h.AgentFn = killedBySIGTERM(nil)

	res := Review(h, stageCfg(), stageLog(t, "PROJ-2"), Args{Identifier: "PROJ-2"})

	if res.OK || res.Disposition != Interrupted || res.Signal != syscall.SIGTERM {
		t.Fatalf("a SIGTERMed review is Interrupted, got %+v", res)
	}
	for _, s := range shellLabels(h.Shells) {
		if strings.HasPrefix(s, gateStep) {
			t.Errorf("shell runs = %v, want no host gate after an interruption", shellLabels(h.Shells))
		}
	}
	assertNothingPushed(t, h, repo)
}

// #99: a signal-killed retrospective is not "never ran": it reports Interrupted.
func TestRetrospectiveReportsASignalKilledSessionAsInterrupted(t *testing.T) {
	h := shipped(t, "PROJ-3")
	h.AgentFn = killedBySIGTERM(nil)
	log := stageLog(t, "PROJ-3")

	res := Retrospective(h, stageCfg(), log, Args{Identifier: "PROJ-3"})

	if res.OK || res.Disposition != Interrupted {
		t.Fatalf("a SIGTERMed retrospective is Interrupted, got %+v", res)
	}
	if strings.Contains(narration(t, log), "retrospective ✗") {
		t.Errorf("an interruption is not a failed retrospective; run.jsonl = %s", narration(t, log))
	}
}
