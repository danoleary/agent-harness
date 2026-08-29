package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The sandbox image bakes a Playwright Chromium pinned by the Dockerfile's
// PLAYWRIGHT_VERSION ARG (BEH-405). That pin only works if it matches the
// `playwright`/`@playwright/test` version web actually runs: a drift makes the
// runner look for a browser revision the baked one isn't, reviving the exact
// "Executable doesn't exist at …headless_shell" failure the bake was added to
// kill. The Dockerfile comment says the two "must track" each other — this test
// turns that prose invariant into an enforced one so a lone `@playwright/test`
// bump fails CI instead of silently breaking the in-sandbox gates.
func TestPlaywrightVersionMatchesWebPackage(t *testing.T) {
	root := repoRoot(t)

	dockerfile := mustRead(t, filepath.Join(root, "agent-harness", "Dockerfile"))
	pkg := mustRead(t, filepath.Join(root, "web", "package.json"))

	argRe := regexp.MustCompile(`(?m)^ARG\s+PLAYWRIGHT_VERSION=([0-9][^\s]*)\s*$`)
	argMatch := argRe.FindStringSubmatch(dockerfile)
	if argMatch == nil {
		t.Fatal("could not find `ARG PLAYWRIGHT_VERSION=<version>` in agent-harness/Dockerfile")
	}
	baked := argMatch[1]

	// Both the runner (@playwright/test) and the browser-installer (playwright)
	// must equal the baked version — they're installed together and resolve the
	// same browser revision.
	for _, dep := range []string{"@playwright/test", "playwright"} {
		depRe := regexp.MustCompile(`"` + regexp.QuoteMeta(dep) + `":\s*"([^"]+)"`)
		depMatch := depRe.FindStringSubmatch(pkg)
		if depMatch == nil {
			t.Fatalf("could not find %q in web/package.json", dep)
		}
		if got := depMatch[1]; got != baked {
			t.Errorf("Playwright version drift: Dockerfile PLAYWRIGHT_VERSION=%s but web/package.json %s=%s — bump the Dockerfile ARG to match, or the sandbox bakes a browser revision the gates won't find (BEH-405)", baked, dep, got)
		}
	}
}

// TestPlaywrightVersionMatchesWebPackage guards the pin, but a guard only helps if
// CI actually runs it on the change it catches. The Agent Harness workflow is
// path-filtered to `agent-harness/**` + `.agents/skills/**`, so a lone
// `@playwright/test` bump under `web/` triggered NO harness job and the guard stayed
// silent — exactly how the 1.60.0→1.61.1 drift shipped and only surfaced at runtime
// in BEH-769's sandbox (Executable doesn't exist at …headless_shell). The workflow
// already re-triggers on `.agents/skills/**` by identical logic (its skill-contract
// tests read those files). Since this guard reads `web/package.json`, that file must
// likewise be a trigger path — on BOTH the pull_request and push events — so a
// playwright bump re-runs the guard instead of merging blind (BEH-776).
func TestHarnessCIRetriggersOnWebPackageBump(t *testing.T) {
	root := repoRoot(t)
	wf := mustRead(t, filepath.Join(root, ".github", "workflows", "agent-harness.yaml"))

	pathsRe := regexp.MustCompile(`(?m)^\s*paths:\s*\[(.*)\]\s*$`)
	matches := pathsRe.FindAllStringSubmatch(wf, -1)
	if len(matches) < 2 {
		t.Fatalf("expected >=2 `paths:` trigger lines (pull_request + push) in agent-harness.yaml, found %d", len(matches))
	}
	for _, m := range matches {
		if !strings.Contains(m[1], "web/package.json") {
			t.Errorf("agent-harness.yaml `paths:` trigger [%s] omits web/package.json — a lone @playwright/test bump under web/ would skip the TestPlaywrightVersionMatchesWebPackage guard and the browser-revision drift ships silently (BEH-776)", m[1])
		}
	}
}

// repoRoot walks up from this test file to the repo root — the dir that holds
// both the harness Dockerfile and the web package the pin must track.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		_, dErr := os.Stat(filepath.Join(dir, "agent-harness", "Dockerfile"))
		_, pErr := os.Stat(filepath.Join(dir, "web", "package.json"))
		if dErr == nil && pErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (agent-harness/Dockerfile + web/package.json) above " + thisFile)
		}
		dir = parent
	}
}
