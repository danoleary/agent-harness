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

// #368: a session that stopped blocked on a human, cleanly, is no record that
// there is no work. The loop labels the ticket blocked, and a human removing the
// label restores the labels selection read it with: a record kept would pass the
// ticket over until main moved, though the label's removal is the signal to work
// it again.
func TestImplementationDoesNotRecordASessionBlockedOnAHumanAsNoWork(t *testing.T) {
	h, _ := fresh(t, "#368")
	cfg := sharedCfg(t)
	records := nowork.For(cfg)
	var dir string
	h.AgentFn = writesBlocked(t, blockedMarker, &dir, nil)

	res := Implementation(h, cfg, stageLog(t, "#368"), Args{Identifier: "#368", Selected: selectedAs})

	if res.Disposition != SessionBlocked {
		t.Fatalf("got %+v, want SessionBlocked", res)
	}
	if rec, ok := records.Read("#368"); ok {
		t.Errorf("record = %+v, want none: removing the blocked label must let the ticket be selected again", rec)
	}
}

func exits(o session.Outcome) func(*hostio.Fake, *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
	return func(*hostio.Fake, *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
		return func(int, hostio.AgentRun) session.Outcome { return o }
	}
}

// #453: a session that found the ticket already resolved on main by a commit
// records that commit, so selection passes over the ticket while main still has
// it, however often main moves.
func TestImplementationRecordsTheCommitASessionFoundResolvingTheTicket(t *testing.T) {
	h, repo := fresh(t, "PROJ-20")
	repo.Merge("helper.go", "func helper() {}\n", "PROJ-99: add the helper")
	fix := repo.Git(repo.Origin, "rev-parse", "main")
	h.AgentFn = resolvedBy(t, fix[:9])
	cfg := sharedCfg(t)
	log := stageLog(t, "PROJ-20")

	res := Implementation(h, cfg, log, Args{Identifier: "PROJ-20", Selected: selectedAs})

	if res.Disposition != ResolvedOnMain {
		t.Fatalf("got %+v, want ResolvedOnMain", res)
	}
	rec, ok := nowork.For(cfg).Read("PROJ-20")
	if !ok || rec.ResolvedBy != fix || rec.Ticket != selectedAs {
		t.Fatalf("record = %+v, %v; want it resolved by %s", rec, ok, fix)
	}
	if got := narration(t, log); !strings.Contains(got, "already resolved on main by "+fix[:12]+" — it is not dispatched again while main has that commit") {
		t.Errorf("the record must be narrated; run.jsonl = %s", got)
	}
}

// The pre-dispatch own-key scan's skip records the merge commit too: without
// it, every main move dispatched the ticket into the same skip (#453).
func TestImplementationRecordsTheMergeCommitOfWorkAlreadyOnMain(t *testing.T) {
	h, repo := fresh(t, "PROJ-4")
	repo.Merge("fix.txt", "the fix\n", "PROJ-4: the fix")
	merged := repo.OnOrigin("main")
	cfg := sharedCfg(t)

	res := Implementation(h, cfg, stageLog(t, "PROJ-4"), Args{Identifier: "PROJ-4", Selected: selectedAs})

	if res.Disposition != AlreadyOnMain {
		t.Fatalf("got %+v, want AlreadyOnMain", res)
	}
	if rec, ok := nowork.For(cfg).Read("PROJ-4"); !ok || rec.ResolvedBy != merged || rec.Ticket != selectedAs {
		t.Errorf("record = %+v, %v; want it resolved by %s", rec, ok, merged)
	}

	handPassed, repo := fresh(t, "PROJ-4")
	repo.Merge("fix.txt", "the fix\n", "PROJ-4: the fix")
	cfg = sharedCfg(t)
	Implementation(handPassed, cfg, stageLog(t, "PROJ-4"), Args{Identifier: "PROJ-4"})
	if rec, ok := nowork.For(cfg).Read("PROJ-4"); ok {
		t.Errorf("a hand-passed run records nothing, got %+v", rec)
	}
}

// #480, replayed: a run found the ticket already resolved on main, and ten hours
// later the ticket was dispatched again with a prompt byte-identical to the
// first. The session re-derived the verdict from scratch and named a different
// commit. Now a run whose ticket carries a resolved verdict is told it, so it can
// confirm it in one step; a plain no-work record tells it nothing.
func TestImplementationTellsTheSessionAPriorResolvedVerdict(t *testing.T) {
	const fix = "5e1f0c2d9b8a7f6e5d4c3b2a1908f7e6d5c4b3a2"
	const why = "the skip guard already keys the verdict on the ticket"
	for name, c := range map[string]struct {
		write func(nowork.Records) error
		told  bool
	}{
		"resolved": {write: func(r nowork.Records) error { return r.WriteResolved("#480", "base", "fp", fix, why) }, told: true},
		"no work":  {write: func(r nowork.Records) error { return r.Write("#480", "base", "fp") }},
	} {
		h, repo := fresh(t, "#480")
		cfg := sharedCfg(t)
		if err := c.write(nowork.For(cfg)); err != nil {
			t.Fatal(err)
		}
		h.AgentFn = commitsHandoff(h, repo)
		log := stageLog(t, "#480")

		Implementation(h, cfg, log, Args{Identifier: "#480", Selected: selectedAs})

		if len(h.Agents) != 1 {
			t.Fatalf("%s: runs = %v, want one session", name, labels(h.Agents))
		}
		p := h.Agents[0].Prompt
		if got := strings.Contains(p, "found this ticket already resolved on `main` by commit `"+fix+"`") && strings.Contains(p, why); got != c.told {
			t.Errorf("%s: prompt names the prior verdict = %v, want %v:\n%s", name, got, c.told, p)
		}
		if got := strings.Contains(narration(t, log), "#480's last run found it already resolved on main by "+fix[:12]); got != c.told {
			t.Errorf("%s: run log names the prior verdict = %v, want %v", name, got, c.told)
		}
	}
}
