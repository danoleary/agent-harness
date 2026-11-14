package stages

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/session"
)

// selectedAs is the fingerprint selection read the ticket as.
const selectedAs nowork.Fingerprint = "selected-fingerprint"

// #330, replayed: the session read the ticket, found the fix already on
// origin/main from another PR, said so and exited 0 without a commit. The stage
// records the origin/main SHA the run worked against and the ticket as selection
// read it, so the next selection passes over the ticket while neither changed.
func TestImplementationRecordsARunThatFoundNothingToHandOff(t *testing.T) {
	h, repo := fresh(t, "#330")
	cfg := sharedCfg(t)
	// The session's last word was that #330 is already resolved on origin/main by
	// another PR, so it had nothing to commit.
	h.AgentFn = func(int, hostio.AgentRun) session.Outcome { return session.Outcome{} }
	log := stageLog(t, "#330")

	res := Implementation(h, cfg, log, Args{Identifier: "#330", Selected: selectedAs})

	if res.OK {
		t.Fatal("no handoff commit is still a failed verdict")
	}
	main, err := repo.Checkout().MainSHA()
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := nowork.For(cfg).Read("#330")
	if !ok || rec.Base != main || rec.Ticket != selectedAs {
		t.Fatalf("record = %+v, %v; want base %s and the selected fingerprint", rec, ok, main)
	}
	if got := narration(t, log); !strings.Contains(got, "no work against origin/main "+main[:12]) {
		t.Errorf("the record must be narrated; run.jsonl = %s", got)
	}
}

// Only a session that ran to completion and found nothing is recorded. A
// handoff, a crash, a refusal or a cap says nothing about whether there is work,
// and clears an older record; a hand-passed run, which selection did not read,
// records nothing.
func TestImplementationRecordsOnlyASessionThatFoundNothing(t *testing.T) {
	for name, c := range map[string]struct {
		session  func(*hostio.Fake, *gittest.Repo) func(int, hostio.AgentRun) session.Outcome
		selected nowork.Fingerprint
	}{
		"a handoff":   {selected: selectedAs, session: commitsHandoff},
		"a crash":     {selected: selectedAs, session: exits(session.Outcome{ExitCode: 137})},
		"a refusal":   {selected: selectedAs, session: exits(session.Outcome{ExitCode: 1, UsagePolicyRefusal: true})},
		"a cap":       {selected: selectedAs, session: exits(session.Outcome{SpendingCapAbort: true})},
		"a $0 run":    {selected: selectedAs, session: exits(session.Outcome{NoRealTurns: true})},
		"hand-passed": {session: exits(session.Outcome{})},
	} {
		h, repo := fresh(t, "#330")
		cfg := sharedCfg(t)
		records := nowork.For(cfg)
		if err := records.Write("#330", "older-base", "older"); err != nil {
			t.Fatal(err)
		}
		h.AgentFn = c.session(h, repo)

		Implementation(h, cfg, stageLog(t, "#330"), Args{Identifier: "#330", Selected: c.selected})

		if rec, ok := records.Read("#330"); ok {
			t.Errorf("%s: record = %+v, want none", name, rec)
		}
	}
}

func exits(o session.Outcome) func(*hostio.Fake, *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
	return func(*hostio.Fake, *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
		return func(int, hostio.AgentRun) session.Outcome { return o }
	}
}
