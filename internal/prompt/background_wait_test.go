package prompt

import (
	"strings"
	"testing"
)

// #426: a review session ran its gate with run_in_background and also armed a
// Monitor on `tail -f <log> | grep --line-buffered ...`. The gate finished and the
// session reached end_turn, but `tail -f` never exits, so the CLI kept the session
// alive until the Monitor's 10-minute expiry: about 7 minutes of a 25-minute cap
// passed with no work. Every Stage's envelope now says to wait on the background
// task's own completion notice, never to use `tail -f` as a Monitor source, and to
// give any Monitor a source that ends when the run ends.
func TestEveryStageSteersOffANeverEndingMonitorSource(t *testing.T) {
	bodies := map[Stage]string{Implement: "implement", Review: "review", Retrospective: "retro"}
	for _, st := range []Stage{Implement, Review, Retrospective, CIFix, RebaseFix, SecurityFix, Triage} {
		for _, v := range []struct {
			r     Resume
			merge bool
		}{{Fresh, false}, {ResumedBranch, false}, {AfterRefusal, false}, {Fresh, true}} {
			r := v.r
			c := Context{Slug: "gh-426", BranchPrefix: testPrefix, WorktreePath: sampleWorktree, Resume: r, CILogs: sampleCILogs, Merge: v.merge}
			if b, ok := bodies[st]; ok {
				c.Body = exampleBody(t, b)
			}
			p := For(st, c)
			for _, want := range []string{
				"run_in_background",
				"completion notification",
				"never use `tail -f`",
				"until-loop",
				"exits on the run's summary line or its failure line",
			} {
				if !strings.Contains(p, want) {
					t.Errorf("stage %d resume %d merge %v prompt missing %q", st, r, v.merge, want)
				}
			}
		}
	}
}

// The steer is trusted envelope prose, appended un-defanged like bashQuirkSteer,
// so it must carry no live `!`+backtick directive (BEH-709).
func TestBackgroundWaitSteerCarriesNoLiveDirective(t *testing.T) {
	if strings.Contains(backgroundWaitSteer, bangBacktick) {
		t.Errorf("backgroundWaitSteer contains a live %q directive opener", bangBacktick)
	}
}
