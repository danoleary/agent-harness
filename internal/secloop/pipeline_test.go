package secloop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// The lockfiles a SecurityFix session finds on main and leaves behind.
const (
	vulnerableLock = "lockfileVersion: '9.0'\n\npackages:\n\n  lodash@4.17.20:\n    resolution: {integrity: sha512-old}\n"
	patchedLock    = "lockfileVersion: '9.0'\n\npackages:\n\n  lodash@4.17.21:\n    resolution: {integrity: sha512-new}\n"
)

func securityLoop() config.Loop {
	return config.Loop{
		Name:   "security-alerts",
		Source: config.LoopSource{Kind: config.SourceGitHubSecurityAlerts, Severity: []string{"critical", "high"}, MaxAttempts: 2},
		Steps: []config.Step{
			{Use: config.StepNewBranch}, {Use: config.StepFixAlerts}, {Use: config.StepGates}, {Use: config.StepOpenPR},
		},
	}
}

func securityCfg(t *testing.T) config.Config {
	return config.Config{
		Host: config.Host{ProjectPath: t.TempDir(), ReviewTimeout: 30 * time.Minute, RetrospectiveTimeout: 20 * time.Minute},
		Project: config.Project{
			BranchPrefix: gittest.Prefix,
			PostCreate:   "pnpm install --frozen-lockfile",
			Gates:        []config.Gate{{Name: "check", Command: "pnpm check"}},
		},
	}
}

func lodashItem() Item {
	g := Group{
		Ecosystem: "npm", Package: "lodash",
		Alerts: []github.Alert{
			{Number: 7, URL: "https://github.com/acme/widgets/security/dependabot/7", Ecosystem: "npm", Package: "lodash", ManifestPath: "package.json", Severity: "high", GHSA: "GHSA-35jh-r3h4-6jhm", CVE: "CVE-2021-23337", Summary: "Command Injection in lodash", VulnerableRange: "< 4.17.21", Patched: "4.17.21"},
			{Number: 9, URL: "https://github.com/acme/widgets/security/dependabot/9", Ecosystem: "npm", Package: "lodash", ManifestPath: "pnpm-lock.yaml", Severity: "critical", GHSA: "GHSA-p6mc-m468-83gw", Summary: "Prototype Pollution in lodash", VulnerableRange: ">= 4.0.0, < 4.17.20", Patched: "4.17.20"},
		},
		Unpatched: []github.Alert{
			{Number: 11, URL: "https://github.com/acme/widgets/security/dependabot/11", Ecosystem: "npm", Package: "lodash", ManifestPath: "package.json", Severity: "high", GHSA: "GHSA-xxxx-yyyy-zzzz", Summary: "ReDoS in lodash", VulnerableRange: ">= 4.0.0, <= 4.17.21"},
		},
	}
	return Item{Group: g, Branch: gittest.Prefix + "/" + ticket.Slug(g.Key()), Fingerprint: Fingerprint(g)}
}

// secHost is a healthy Fake for it over a real repo whose main resolves a
// vulnerable lodash, and whose SecurityFix session commits a patched lockfile.
func secHost(t *testing.T, it Item) (*hostio.Fake, *gittest.Repo) {
	t.Helper()
	repo := gittest.New(t)
	repo.Merge("pnpm-lock.yaml", vulnerableLock, "lock lodash")
	h := hostio.NewFake(repo.Checkout(), ticket.Slug(it.Key()))
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "securityfix" {
			repo.Commit(h.Worktree(), "pnpm-lock.yaml", patchedLock, "fix(deps): patch lodash")
		}
		return h.AgentOutcome
	}
	return h, repo
}

func secLog(t *testing.T) *runlog.Logger {
	t.Helper()
	log, err := runlog.New(t.TempDir(), "SEC-npm-lodash")
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return log
}

func calls(h *hostio.Fake) string { return strings.Join(h.Calls, "|") + "|" }

// The happy path: cut the branch fresh, provision it, run the SecurityFix session,
// check its lockfile on the host, gate it, and land it as a new PR carrying the
// fingerprint marker and the held label.
func TestFixLandsACheckedGateGreenFixAsANewPR(t *testing.T) {
	it := lodashItem()
	h, repo := secHost(t, it)
	cfg := securityCfg(t)

	res := Fix(h, cfg, secLog(t), it, securityLoop())

	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a checked, gate-green fix ships, got %+v", res)
	}
	got := calls(h)
	order := []string{"preflight|", "fetch-main|", "shell prep|", "agent securityfix|", "shell gate-check|", "push-force|", "create-pr fix(deps): patch lodash (2 critical/high alerts)|", "label-pr in-progress|"}
	at := 0
	for _, want := range order {
		i := strings.Index(got[at:], want)
		if i < 0 {
			t.Fatalf("calls missing %q in order:\n%v", want, h.Calls)
		}
		at += i + len(want)
	}
	if repo.OnOrigin(it.Branch) == "" {
		t.Errorf("the fix must reach origin on %s", it.Branch)
	}
	if len(h.PRBodies) != 1 {
		t.Fatalf("one PR opened, got %d", len(h.PRBodies))
	}
	body := h.PRBodies[0]
	for _, want := range []string{
		Marker(it.Fingerprint),
		"[#7](https://github.com/acme/widgets/security/dependabot/7)",
		"[#9](https://github.com/acme/widgets/security/dependabot/9)",
		"GHSA-35jh-r3h4-6jhm",
		"< 4.17.21",
		"[#11](https://github.com/acme/widgets/security/dependabot/11)",
		"fix(deps): patch lodash",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(h.Agents[0].Prompt, "| #7 |") || !strings.Contains(h.Agents[0].Prompt, "`"+it.Branch+"`") {
		t.Errorf("the session gets the alerts and the package's branch; prompt:\n%s", h.Agents[0].Prompt)
	}
}

// Every way a fix fails before the landing pushes nothing.
func TestFixPushesNothingWhenTheFixIsNotProven(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *hostio.Fake, repo *gittest.Repo)
		want  stages.Disposition
		// gated marks a refusal the gates make; the host check refuses the rest
		// before any gate runs.
		gated bool
	}{
		{"red gate", func(h *hostio.Fake, _ *gittest.Repo) {
			h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
				if strings.HasPrefix(run.Label, "gate-") {
					return session.Outcome{ExitCode: 1}
				}
				return session.Outcome{}
			}
		}, stages.NoPR, true},
		{"lockfile still vulnerable", func(h *hostio.Fake, repo *gittest.Repo) {
			h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
				repo.Commit(h.Worktree(), "package.json", `{"dependencies":{"lodash":"^4.17.21"}}`, "bump lodash")
				return h.AgentOutcome
			}
		}, stages.NoPR, false},
		{"non-dependency file touched", func(h *hostio.Fake, repo *gittest.Repo) {
			h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
				repo.Commit(h.Worktree(), "pnpm-lock.yaml", patchedLock, "fix(deps): patch lodash")
				repo.Commit(h.Worktree(), "src/index.js", "fixed()\n", "fix the build")
				return h.AgentOutcome
			}
		}, stages.NoPR, false},
		{"dirty tree", func(h *hostio.Fake, repo *gittest.Repo) {
			h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
				repo.Commit(h.Worktree(), "pnpm-lock.yaml", patchedLock, "fix(deps): patch lodash")
				repo.Dirty(h.Worktree())
				return h.AgentOutcome
			}
		}, stages.NoPR, false},
		{"cap abort", func(h *hostio.Fake, _ *gittest.Repo) {
			h.AgentFn = func(int, hostio.AgentRun) session.Outcome {
				return session.Outcome{ExitCode: 1, SpendingCapAbort: true}
			}
		}, stages.CapAborted, false},
		{"failed preflight", func(h *hostio.Fake, _ *gittest.Repo) {
			h.PreflightErr = errors.New("docker daemon unreachable")
		}, stages.PreflightAborted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := lodashItem()
			h, repo := secHost(t, it)
			tc.setup(h, repo)

			res := Fix(h, securityCfg(t), secLog(t), it, securityLoop())

			if res.OK || res.Disposition != tc.want {
				t.Errorf("got %+v, want not OK and %v", res, tc.want)
			}
			if got := calls(h); strings.Contains(got, "push") || strings.Contains(got, "create-pr") || strings.Contains(got, "label-pr") {
				t.Errorf("nothing may be pushed; calls = %v", h.Calls)
			}
			if repo.OnOrigin(it.Branch) != "" {
				t.Errorf("%s reached origin", it.Branch)
			}
			if gated := strings.Contains(calls(h), "shell gate-"); gated != tc.gated {
				t.Errorf("gates ran = %v, want %v; calls = %v", gated, tc.gated, h.Calls)
			}
		})
	}
}

// A sibling changed the lockfile on main while the fix gated: with no conflict hook
// the landing pushes nothing, and the attempt waits for the next main.
func TestFixPushesNothingOnALockfileConflict(t *testing.T) {
	it := lodashItem()
	h, repo := secHost(t, it)
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, "gate-") {
			repo.Merge("pnpm-lock.yaml", strings.Replace(vulnerableLock, "sha512-old", "sha512-other", 1), "a sibling relocked")
		}
		return session.Outcome{}
	}
	cfg := securityCfg(t)

	res := Fix(h, cfg, secLog(t), it, securityLoop())

	if res.OK || res.Disposition != stages.NoPR {
		t.Errorf("a conflicted landing is an unpushed attempt, got %+v", res)
	}
	if strings.Contains(calls(h), "push") || repo.OnOrigin(it.Branch) != "" {
		t.Errorf("a conflicted landing pushes nothing; calls = %v", h.Calls)
	}
	if t2 := tallyOf(ledgerOf(t, cfg), it.Key(), it.Fingerprint); t2.resolved {
		t.Error("a conflict is not a resolution")
	}
}

// Main fixed the package while the fix gated: the replay collapses the branch, so
// nothing is pushed, and the ledger records the fingerprint resolved.
func TestFixRecordsACollapsedLandingAsResolved(t *testing.T) {
	it := lodashItem()
	h, repo := secHost(t, it)
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, "gate-") {
			repo.Merge("pnpm-lock.yaml", patchedLock, "a sibling patched lodash")
		}
		return session.Outcome{}
	}
	cfg := securityCfg(t)

	res := Fix(h, cfg, secLog(t), it, securityLoop())

	if res.Disposition != stages.RecommendClose {
		t.Errorf("a collapsed landing recommends close, got %+v", res)
	}
	if strings.Contains(calls(h), "push") || repo.OnOrigin(it.Branch) != "" {
		t.Errorf("a collapsed landing pushes nothing; calls = %v", h.Calls)
	}
	if !tallyOf(ledgerOf(t, cfg), it.Key(), it.Fingerprint).resolved {
		t.Error("the ledger must record the fingerprint resolved")
	}
}

// A failed attempt left the package's branch behind with its commits: the next
// attempt starts from origin/main, not from them.
func TestFixCutsTheBranchFreshOverAFailedAttempt(t *testing.T) {
	it := lodashItem()
	h, repo := secHost(t, it)
	wt := h.Worktree()
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	repo.Commit(wt, "src/index.js", "a failed attempt\n", "a failed attempt")

	res := Fix(h, securityCfg(t), secLog(t), it, securityLoop())

	if !res.OK {
		t.Fatalf("the fresh attempt ships, got %+v", res)
	}
	if log := repo.Git(repo.Origin, "log", "--format=%s", it.Branch); strings.Contains(log, "a failed attempt") {
		t.Errorf("the failed attempt's commit came back:\n%s", log)
	}
}

func ledgerOf(t *testing.T, cfg config.Config) []Entry {
	t.Helper()
	entries, err := readLedger(filepath.Join(OptionsFrom(cfg, securityLoop()).LogsDir, LedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// A Go module's fix runs the same pipeline: the host scans the worktree's go.mod,
// and a committed go.mod and go.sum that require no vulnerable version land as a
// new PR whose body names go.mod as what was checked. One that still requires a
// vulnerable version is refused and nothing is pushed.
func TestFixLandsAGoModuleFix(t *testing.T) {
	const (
		vulnerableMod = "module example.com/app\n\ngo 1.22\n\nrequire golang.org/x/net v0.17.0 // indirect\n"
		patchedMod    = "module example.com/app\n\ngo 1.22\n\nrequire golang.org/x/net v0.23.0 // indirect\n"
	)
	g := Group{Ecosystem: "go", Package: "golang.org/x/net", Alerts: []github.Alert{
		{Number: 5, URL: "https://github.com/acme/widgets/security/dependabot/5", Ecosystem: "go", Package: "golang.org/x/net", ManifestPath: "go.mod", Severity: "high", GHSA: "GHSA-4v7x-pqxf-cx7m", Summary: "HTTP/2 CONTINUATION flood", VulnerableRange: "< 0.23.0", Patched: "0.23.0"},
	}}
	it := Item{Group: g, Branch: gittest.Prefix + "/" + ticket.Slug(g.Key()), Fingerprint: Fingerprint(g)}
	for _, tc := range []struct {
		name    string
		fixed   string
		shipped bool
	}{
		{"patched", patchedMod, true},
		{"still vulnerable", vulnerableMod + "\n// touched\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := gittest.New(t)
			repo.Merge("go.mod", vulnerableMod, "require x/net")
			h := hostio.NewFake(repo.Checkout(), ticket.Slug(it.Key()))
			h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
				if run.Label == "securityfix" {
					repo.Commit(h.Worktree(), "go.mod", tc.fixed, "fix(deps): patch golang.org/x/net")
				}
				return h.AgentOutcome
			}
			cfg := securityCfg(t)
			cfg.PostCreate = "go mod download"
			cfg.Gates = []config.Gate{{Name: "check", Command: "go test ./..."}}

			res := Fix(h, cfg, secLog(t), it, securityLoop())

			if res.OK != tc.shipped {
				t.Fatalf("OK = %v, want %v: %+v\n%v", res.OK, tc.shipped, res, h.Calls)
			}
			if !tc.shipped {
				if repo.OnOrigin(it.Branch) != "" || len(h.PRBodies) != 0 {
					t.Error("a fix the host check refused must push nothing")
				}
				return
			}
			if len(h.PRBodies) != 1 || !strings.Contains(h.PRBodies[0], "The host checked that `go.mod` resolves no version") {
				t.Errorf("PR body should name go.mod as checked:\n%v", h.PRBodies)
			}
		})
	}
}

// watchingLoop is the built-in security-alerts loop: the required Steps, then the
// CI watch and the retrospective.
func watchingLoop() config.Loop {
	l := securityLoop()
	l.Steps = append(l.Steps, config.Step{Use: config.StepWatchCI}, config.Step{Use: config.StepRetro})
	return l
}

// CI goes red once over the opened PR: the CIFix session commits a fix, the gates
// run green over it, the watch pushes it, and CI then goes green. The fix is
// pr-doctor's gated CI fix, and the watch may rebase the harness's own branch.
func TestFixGatesAndPushesACIFixThenWatchesCIGoGreen(t *testing.T) {
	it := lodashItem()
	h, repo := secHost(t, it)
	h.AgentFn = func(_ int, run hostio.AgentRun) session.Outcome {
		switch run.Label {
		case "securityfix":
			repo.Commit(h.Worktree(), "pnpm-lock.yaml", patchedLock, "fix(deps): patch lodash")
		case "cifix-1":
			repo.Commit(h.Worktree(), "src/compat.js", "export const merge = Object.assign\n", "Fix the build against lodash 4.17.21")
		}
		return h.AgentOutcome
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		// Red once: the watch hands the failing logs to Fix, and pushes what it accepts.
		if fixErr = w.Fix("FAIL src/compat.test.js", true); fixErr != nil {
			return ci.Outcome{Reason: "auto-fix refused"}
		}
		if err := h.Push(); err != nil {
			t.Fatal(err)
		}
		return ci.Outcome{OK: true, Reason: "green after the fix"}
	}

	res := Fix(h, securityCfg(t), secLog(t), it, watchingLoop())

	if fixErr != nil {
		t.Fatalf("a committed, gate-green CI fix is accepted, got %v", fixErr)
	}
	if !res.OK || res.Disposition != stages.Shipped {
		t.Fatalf("a PR whose CI went green ships, got %+v", res)
	}
	if len(h.Watches) != 1 || h.Watches[0].KeepHistory {
		t.Errorf("one watch that may rebase the harness's own branch, got %+v", h.Watches)
	}
	got := calls(h)
	order := []string{"create-pr ", "watch-ci|", "agent cifix-1|", "shell gate-cifix1-check|", "push|", "agent retrospective|"}
	at := 0
	for _, want := range order {
		i := strings.Index(got[at:], want)
		if i < 0 {
			t.Fatalf("calls missing %q in order:\n%v", want, h.Calls)
		}
		at += i + len(want)
	}
	if !strings.Contains(h.Agents[1].Prompt, "FAIL src/compat.test.js") || !strings.Contains(h.Agents[1].Prompt, "`"+it.Branch+"`") {
		t.Errorf("the CIFix session gets the failing logs and the package's branch; prompt:\n%s", h.Agents[1].Prompt)
	}
	if log := repo.Git(repo.Origin, "log", "-1", "--format=%s", it.Branch); log != "Fix the build against lodash 4.17.21" {
		t.Errorf("the gated fix must reach origin, got tip %q", log)
	}
	if _, err := os.Stat(h.Worktree().Path()); !os.IsNotExist(err) {
		t.Errorf("a clean worktree is removed after the run, stat = %v", err)
	}
}

// A red gate over the CIFix commit refuses the fix: the watch pushes nothing more,
// and the opened PR still counts as shipped, left to pr-doctor once the held
// label comes off.
func TestFixRefusesACIFixWhoseGatesFail(t *testing.T) {
	it := lodashItem()
	h, repo := secHost(t, it)
	h.ShellFn = func(_ int, run hostio.ShellRun) session.Outcome {
		if strings.HasPrefix(run.Label, "gate-cifix") {
			return session.Outcome{ExitCode: 2}
		}
		return session.Outcome{}
	}
	var fixErr error
	h.CIFn = func(w hostio.CIWatch) ci.Outcome {
		fixErr = w.Fix("FAIL", true)
		return ci.Outcome{Reason: "auto-fix refused"}
	}

	res := Fix(h, securityCfg(t), secLog(t), it, watchingLoop())

	if fixErr == nil || !strings.Contains(fixErr.Error(), `gate "check"`) {
		t.Fatalf("a fix over a red gate must be refused naming the gate, got %v", fixErr)
	}
	if res.Disposition != stages.Shipped {
		t.Errorf("the PR is open, so the run shipped, got %+v", res)
	}
	if log := repo.Git(repo.Origin, "log", "-1", "--format=%s", it.Branch); log != "fix(deps): patch lodash" {
		t.Errorf("only the security fix is on origin, got tip %q", log)
	}
}

// The retrospective files its project findings into the tracker's findings team:
// no ticket names one for a package (ADR-0038).
func TestFixFilesProjectFindingsIntoTheTrackersTeam(t *testing.T) {
	it := lodashItem()
	h, _ := secHost(t, it)
	h.Trk.Team = "beh-team-uuid"

	Fix(h, securityCfg(t), secLog(t), it, watchingLoop())

	if strings.Join(h.Routed, ",") != it.Key() || strings.Join(h.RoutedTeams, ",") != "beh-team-uuid" {
		t.Errorf("routed %v into teams %v, want %s into beh-team-uuid", h.Routed, h.RoutedTeams, it.Key())
	}
}
