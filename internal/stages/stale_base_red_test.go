package stages

import (
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/basegate"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// openBaseRed is the tracker state of #311: a base-gates-red finding filed while
// main was red and still open, beside a closed one and an unrelated finding.
func openBaseRed() []tracker.ExistingFinding {
	return []tracker.ExistingFinding{
		{Identifier: "#280", Title: "main fails gate \"check\" before any change, so every ticket on it is held", Key: basegate.FindingKey},
		{Identifier: "#200", Title: "an older red base", Key: basegate.FindingKey, Closed: true},
		{Identifier: "#281", Title: "Build OOMs", Key: "sandbox-build-oom"},
	}
}

// #311: a run that gates its base green says so, once, on the open base-gates-red
// finding, so a human knows it can be closed. A later green base, after main
// moves, does not say it again on the same issue. The harness never closes it.
func TestAGreenBaseTellsTheOpenBaseRedFindingOnce(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)

	first := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-30"))
	first.Trk.Findings = openBaseRed()
	first.AgentFn = commitsHandoff(first, repo)
	res := Implementation(first, cfg, stageLog(t, "PROJ-30"), Args{Identifier: "PROJ-30"})
	if !res.OK {
		t.Fatalf("a green base must dispatch, got %+v", res)
	}
	base := repo.OnOrigin("main")
	if got := first.Trk.CommentedOn; !reflect.DeepEqual(got, []string{"#280"}) {
		t.Fatalf("commented on %v, want the one open base-gates-red finding #280", got)
	}
	if body := first.Trk.Comments[0]; !strings.Contains(body, "base gates green at `"+base+"`") {
		t.Errorf("comment = %q, want it to say the base gates are green at %s", body, base)
	}

	repo.Merge("other.txt", "moved\n", "Move main on")
	second := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-31"))
	second.Trk.Findings = openBaseRed()
	second.AgentFn = commitsHandoff(second, repo)
	if res := Implementation(second, cfg, stageLog(t, "PROJ-31"), Args{Identifier: "PROJ-31"}); !res.OK {
		t.Fatalf("a green base must dispatch, got %+v", res)
	}
	if got := gateRuns(second, baseGateLabel); got != 1 {
		t.Fatalf("gated the moved base %d times, want once", got)
	}
	if got := second.Trk.CommentedOn; len(got) != 0 {
		t.Errorf("commented on %v again, want #280 told only once", got)
	}

	for _, h := range []*hostio.Fake{first, second} {
		for _, k := range append(h.Trk.Canceled, h.Trk.Released...) {
			if k == "#280" {
				t.Errorf("the harness moved the open finding #280 (%v); a human closes it", k)
			}
		}
	}
}

// A red base says nothing on the open finding: it files or recurs as before.
func TestARedBaseDoesNotTellTheOpenBaseRedFindingAnything(t *testing.T) {
	repo := gittest.New(t)
	h := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-32"))
	h.Trk.Findings = openBaseRed()
	redOnBase(h)

	if res := Implementation(h, sharedCfg(t), stageLog(t, "PROJ-32"), Args{Identifier: "PROJ-32"}); res.Disposition != BaseRed {
		t.Fatalf("want the ticket held on the red base, got %+v", res)
	}
	if got := h.Trk.CommentedOn; len(got) != 0 {
		t.Errorf("commented on %v, want nothing said on a red base", got)
	}
}

// The implementation stage reports the base it found green, from a fresh gate
// or a recorded verdict, so the run's retrospective can unsettle base-gates-red.
func TestImplementationReportsTheGreenBase(t *testing.T) {
	repo := gittest.New(t)
	cfg := sharedCfg(t)
	for _, key := range []string{"PROJ-33", "PROJ-34"} {
		h := hostio.NewFake(repo.Checkout(), ticket.Slug(key))
		h.AgentFn = commitsHandoff(h, repo)
		res := Implementation(h, cfg, stageLog(t, key), Args{Identifier: key})
		if want := repo.OnOrigin("main"); res.GreenBase != want {
			t.Errorf("%s: GreenBase = %q, want the base gated green %q", key, res.GreenBase, want)
		}
	}
}

// #311: the retrospective of a run whose base was green leaves base-gates-red out
// of the settled classes and the dedup set; with no green base it changes nothing.
func TestRetrospectiveUnsettlesBaseRedOnAGreenBase(t *testing.T) {
	for _, c := range []struct {
		name      string
		greenBase string
		want      []string
	}{
		{"green base", "abc123", []string{basegate.FindingKey}},
		{"red or no verdict", "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := shipped(t, "PROJ-35")
			h.AgentFn = writesDropbox("[]")

			if res := Retrospective(h, stageCfg(), stageLog(t, "PROJ-35"), Args{Identifier: "PROJ-35", GreenBase: c.greenBase}); !res.OK {
				t.Fatalf("retrospective failed: %+v", res)
			}
			if got := h.Unsettled; !reflect.DeepEqual(got, [][]string{c.want, c.want}) {
				t.Errorf("unsettled per AlreadyFiled/RouteFindings = %v, want %v for both", got, c.want)
			}
		})
	}
}
