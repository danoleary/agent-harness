package verify

import (
	"fmt"
	"path"
	"strings"

	"github.com/danoleary/agent-harness/internal/depcheck"
	"github.com/danoleary/agent-harness/internal/session"
)

// dependencyFiles are the only files a SecurityFix session may change, by base
// name: a manifest, the pnpm lockfile, and the workspace file that holds
// overrides (ADR-0040, decision 4). A nested workspace's package.json counts.
var dependencyFiles = map[string]bool{
	"package.json":        true,
	"pnpm-lock.yaml":      true,
	"pnpm-workspace.yaml": true,
}

// SecurityFix decides whether a SecurityFix session fixed its package's alerts,
// judged on ground truth rather than the session's report (ADR-0040, decision 4).
// It requires a clean exit, a clean tree, at least one commit ahead of origin/main,
// a non-empty diff, changes to dependency files only, and a lockfile that resolves
// no version inside any of the alerts' vulnerable ranges.
//
// chk is the host's scan of the branch's lockfile (depcheck.Check). It is not git,
// so it is not [GroundTruth]; a scan that could not run fails the fix closed, as
// does the zero Result, which no scan produced.
//
// A spending-cap abort wins outright, as in [RebaseResolution]: the session did no
// work, and the caller defers rather than counting a failed attempt.
func SecurityFix(g GroundTruth, out session.Outcome, chk depcheck.Result) Result {
	if out.SpendingCapAbort {
		return Result{SpendingCapAbort: true, Reason: "security-fix session aborted before running — spending cap reached, retry after reset"}
	}
	if out.ExitCode != 0 {
		return Result{Reason: fmt.Sprintf("security-fix session exited %d", out.ExitCode)}
	}
	if !g.Clean() {
		return Result{Reason: "security-fix session left uncommitted changes in the worktree — not pushing"}
	}
	if g.CommitsAhead() < 1 {
		return Result{Reason: "security-fix session made no commit ahead of origin/main — it found no fix"}
	}
	if g.DiffEmpty() {
		return Result{Reason: "security-fix branch makes no net change against origin/main (empty diff) — nothing to ship"}
	}
	changed, err := g.ChangedFiles()
	if err != nil {
		return Result{Reason: "could not list the files the security fix changed: " + err.Error()}
	}
	var other []string
	for _, f := range changed {
		if !dependencyFiles[path.Base(f)] {
			other = append(other, f)
		}
	}
	if len(other) > 0 {
		return Result{Reason: "security fix changed files other than package.json, pnpm-lock.yaml and pnpm-workspace.yaml: " + strings.Join(other, ", ") + " — not pushing"}
	}
	switch {
	case chk.Package == "":
		return Result{Reason: "the lockfile was not checked against the alerts' vulnerable ranges — not pushing"}
	case chk.Err != nil:
		return Result{Reason: "could not check the lockfile for " + chk.Package + ": " + chk.Err.Error() + " — not pushing"}
	case len(chk.Vulnerable) > 0:
		versions := make([]string, len(chk.Vulnerable))
		for i, v := range chk.Vulnerable {
			versions[i] = v.String()
		}
		return Result{Reason: "the lockfile still resolves " + chk.Package + " " + strings.Join(versions, ", ") + " inside a vulnerable range — not pushing"}
	}
	return Result{OK: true, Reason: fmt.Sprintf("security fix committed (%s) and the lockfile resolves no vulnerable %s", strings.Join(changed, ", "), chk.Package)}
}
