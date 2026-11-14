package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// entrypointAsSession is entrypoint.sh's unprivileged pass, run in workDir with
// home as the session user's home, exec-ing `true` where the session would start.
func entrypointAsSession(t *testing.T, workDir, home string) *exec.Cmd {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("entrypoint.sh drops privileges under root; this test runs its unprivileged pass")
	}
	for _, tool := range []string{"bash", "git", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH (the base image has it)", tool)
		}
	}
	cmd := exec.Command("bash", filepath.Join(harnessRoot(t), "entrypoint.sh"), "true")
	cmd.Dir = workDir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"PROJECT_PATH=" + workDir,
		"HARNESS_SESSION_HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
	}
	return cmd
}

// runEntrypointAsSession runs entrypointAsSession and returns the ~/.claude.json
// it leaves.
func runEntrypointAsSession(t *testing.T, workDir, home string) map[string]any {
	t.Helper()
	if out, err := entrypointAsSession(t, workDir, home).CombinedOutput(); err != nil {
		t.Fatalf("entrypoint.sh: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatalf("read ~/.claude.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("~/.claude.json is not JSON: %v\n%s", err, raw)
	}
	return doc
}

// trusted reports whether doc marks dir as a workspace whose trust dialog was accepted.
func trusted(doc map[string]any, dir string) bool {
	projects, _ := doc["projects"].(map[string]any)
	project, _ := projects[dir].(map[string]any)
	return project["hasTrustDialogAccepted"] == true
}

// #305: an untrusted workspace makes the CLI print "Ignoring N permissions.allow
// entries … this workspace has not been trusted" before its stream, which became
// line 1 of every transcript. The CLI keys trust on the session's exact working
// directory (docker's -w: the checkout or the ticket's worktree), not a parent, so
// the entrypoint trusts that one.
func TestEntrypointTrustsTheSessionsWorkDir(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()

	doc := runEntrypointAsSession(t, workDir, home)

	if !trusted(doc, workDir) {
		t.Errorf("~/.claude.json does not trust the workdir %s: %v", workDir, doc)
	}
}

// The file is the CLI's own state (onboarding, caches, other projects): trust is
// merged into it, never written over it.
func TestEntrypointMergesTrustIntoAnExistingClaudeJSON(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	existing := `{"hasCompletedOnboarding":true,"projects":{"/elsewhere":{"allowedTools":["Read"]}}}`
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	doc := runEntrypointAsSession(t, workDir, home)

	if !trusted(doc, workDir) {
		t.Errorf("workdir not trusted: %v", doc)
	}
	if doc["hasCompletedOnboarding"] != true {
		t.Errorf("lost a top-level key: %v", doc)
	}
	if _, ok := doc["projects"].(map[string]any)["/elsewhere"]; !ok {
		t.Errorf("lost another project's entry: %v", doc)
	}
}

// A ~/.claude.json jq cannot parse costs only the warning: the session still
// starts, and the file is left as the CLI wrote it.
func TestEntrypointLeavesAnUnparseableClaudeJSONAlone(t *testing.T) {
	workDir := t.TempDir()
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := entrypointAsSession(t, workDir, home).CombinedOutput(); err != nil {
		t.Fatalf("the session did not start: %v\n%s", err, out)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "{not json" {
		t.Errorf("~/.claude.json = %q, want it untouched", raw)
	}
}
