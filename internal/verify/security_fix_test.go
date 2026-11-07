package verify

import (
	"errors"
	"regexp"
	"testing"

	"github.com/danoleary/agent-harness/internal/depcheck"
	"github.com/danoleary/agent-harness/internal/session"
)

// fixedLodash is a lockfile scan that ran and found no vulnerable lodash.
var fixedLodash = depcheck.Result{Package: "lodash"}

// shipped is the ground truth of a SecurityFix session that did its job: a clean
// tree, one commit ahead, a real diff, and only dependency files changed.
func shipped(t *testing.T) *truth {
	return &truth{t: t, exists: true, clean: true, ahead: 1, changed: []string{"package.json", "pnpm-lock.yaml"}}
}

// The SecurityFix Ground truth (ADR-0040, decision 4): the session's self-report
// is not trusted. Each row breaks one requirement of a passing fix, and its reason
// must say which.
func TestSecurityFixRequiresACommittedDependencyOnlyDiffThatClearsTheLockfile(t *testing.T) {
	vulnerable := depcheck.Result{Package: "lodash", Vulnerable: []depcheck.Version{mustVersion(t, "4.17.20")}}
	cases := []struct {
		name   string
		edit   func(*truth)
		out    session.Outcome
		chk    depcheck.Result
		ok     bool
		reason string
	}{
		{"a direct bump", nil, session.Outcome{}, fixedLodash, true, `lodash`},
		{"an override in a nested workspace", func(g *truth) {
			g.changed = []string{"apps/web/package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"}
		}, session.Outcome{}, fixedLodash, true, `lodash`},
		{"spending cap", nil, session.Outcome{SpendingCapAbort: true}, fixedLodash, false, `spending cap`},
		{"non-zero exit", nil, session.Outcome{ExitCode: 1}, fixedLodash, false, `exited 1`},
		{"dirty tree", func(g *truth) { g.clean = false }, session.Outcome{}, fixedLodash, false, `uncommitted`},
		{"no commit", func(g *truth) { g.ahead = 0 }, session.Outcome{}, fixedLodash, false, `no commit`},
		{"empty diff", func(g *truth) { g.diffEmpty = true }, session.Outcome{}, fixedLodash, false, `empty diff|no net change`},
		{"a source file changed", func(g *truth) {
			g.changed = []string{"package.json", "pnpm-lock.yaml", "src/index.ts"}
		}, session.Outcome{}, fixedLodash, false, `src/index\.ts`},
		{"another lockfile", func(g *truth) {
			g.changed = []string{"package.json", "package-lock.json"}
		}, session.Outcome{}, fixedLodash, false, `package-lock\.json`},
		{"changed files unreadable", func(g *truth) { g.changedErr = errors.New("git broke") }, session.Outcome{}, fixedLodash, false, `git broke`},
		{"lockfile still vulnerable", nil, session.Outcome{}, vulnerable, false, `4\.17\.20`},
		{"lockfile unreadable", nil, session.Outcome{}, depcheck.Result{Package: "lodash", Err: errors.New("no lockfileVersion")}, false, `no lockfileVersion`},
		{"lockfile never checked", nil, session.Outcome{}, depcheck.Result{}, false, `not checked`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := shipped(t)
			if tc.edit != nil {
				tc.edit(g)
			}
			r := SecurityFix(g, tc.out, tc.chk)
			if r.OK != tc.ok {
				t.Errorf("OK = %v, want %v (%s)", r.OK, tc.ok, r.Reason)
			}
			if !regexp.MustCompile(`(?i)` + tc.reason).MatchString(r.Reason) {
				t.Errorf("reason %q should match %q", r.Reason, tc.reason)
			}
		})
	}
}

// A spending-cap abort is the retry-after-reset class, as for RebaseResolution.
func TestSecurityFixFlagsASpendingCapAbort(t *testing.T) {
	if !SecurityFix(shipped(t), session.Outcome{SpendingCapAbort: true}, fixedLodash).SpendingCapAbort {
		t.Error("a cap-aborted session must carry SpendingCapAbort so the caller defers")
	}
}

func mustVersion(t *testing.T, s string) depcheck.Version {
	t.Helper()
	v, err := depcheck.ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
