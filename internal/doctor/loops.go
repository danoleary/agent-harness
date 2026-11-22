package doctor

import (
	"fmt"
	"strings"
)

// checkIgnoredSteps warns, when a [[loops]] entry lists a step its Source no
// longer runs, that the step does nothing: watch_ci on a ticket loop, whose review
// stage ends at the open PR and leaves CI to the PR loop (ADR-0063). The entry
// still loads, so nothing else tells the operator their ticket PRs' red CI now
// waits on a PR loop. doctor cannot tell whether one is running: pr-doctor is a
// daemon of its own. ok is false when no loop lists such a step.
func checkIgnoredSteps(pr project) (Result, bool) {
	if !pr.loaded {
		return Result{}, false
	}
	var lines []string
	for _, l := range pr.cfg.ResolvedLoops {
		for _, step := range l.Ignored {
			lines = append(lines, fmt.Sprintf("loop %q lists %s, which no longer does anything on a ticket loop: its review stage ends once the PR is open, and red CI on its PRs is fixed only by a running PR loop", l.Name, step))
		}
	}
	if len(lines) == 0 {
		return Result{}, false
	}
	return Result{Name: "ticket loop CI", Status: Warn, Detail: strings.Join(lines, "\n"),
		Fix: "remove the step from the loop's steps, and run `agent-harness loop pr-doctor` beside the ticket loop"}, true
}
