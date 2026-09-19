package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePromptBody writes body to <dir>/.agent-harness/prompts/<name>.md, creating
// the directory, and fails the test on any I/O error.
func writePromptBody(t *testing.T, dir, name, body string) {
	t.Helper()
	pDir := filepath.Join(dir, ".agent-harness", "prompts")
	if err := os.MkdirAll(pDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pDir, name+".md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestLoadPromptsReadsPerStageBodies(t *testing.T) {
	dir := t.TempDir()
	writePromptBody(t, dir, "implement", "IMPL BODY")
	writePromptBody(t, dir, "review", "REVIEW BODY")
	writePromptBody(t, dir, "retro", "RETRO BODY")

	pb, err := LoadPrompts(dir)
	if err != nil {
		t.Fatalf("LoadPrompts: %v", err)
	}
	if pb.Implement != "IMPL BODY" {
		t.Errorf("Implement = %q, want IMPL BODY", pb.Implement)
	}
	if pb.Review != "REVIEW BODY" {
		t.Errorf("Review = %q, want REVIEW BODY", pb.Review)
	}
	if pb.Retro != "RETRO BODY" {
		t.Errorf("Retro = %q, want RETRO BODY", pb.Retro)
	}
}

// Every body is REQUIRED (ADR-0009, amended). The harness ships no default skill
// for any Stage, so a missing body leaves that Stage running under the envelope
// alone with nothing to invoke — a session that looks healthy, burns a sandbox,
// a claim and a ticket, and produces nothing. Failing at config load turns that
// into an error the operator reads before the first container starts.
func TestLoadPromptsMissingBodyIsAnError(t *testing.T) {
	dir := t.TempDir() // no .agent-harness/prompts at all

	_, err := LoadPrompts(dir)
	if err == nil {
		t.Fatal("LoadPrompts must error when the Consumer declares no prompt body")
	}
	// One error names every unusable path, so a project adopting the harness
	// fixes all three at once instead of one failed run at a time.
	for _, stage := range []string{"implement.md", "review.md", "retro.md"} {
		if !strings.Contains(err.Error(), stage) {
			t.Errorf("error must name %s; got: %v", stage, err)
		}
	}
	// And it must say what belongs in the file. "Missing prompt body" alone does
	// not tell an operator that naming the skill is now their job.
	if !strings.Contains(strings.ToLower(err.Error()), "skill") {
		t.Errorf("error must point at naming the skill; got: %v", err)
	}
}

// A body that exists but holds only whitespace is the same hole as a missing one:
// the Stage still has nothing to invoke. A `touch implement.md` must not satisfy
// the contract.
func TestLoadPromptsBlankBodyIsAnError(t *testing.T) {
	dir := t.TempDir()
	writePromptBody(t, dir, "implement", "/tdd Work on {{.Identifier}}.")
	writePromptBody(t, dir, "review", "  \n\t\n ")
	writePromptBody(t, dir, "retro", "/retrospective for {{.Identifier}}.")

	_, err := LoadPrompts(dir)
	if err == nil {
		t.Fatal("LoadPrompts must error on a blank prompt body")
	}
	if !strings.Contains(err.Error(), "review.md") {
		t.Errorf("error must name the blank body; got: %v", err)
	}
	// It must not blame a body that is fine, or the operator rewrites the wrong file.
	if strings.Contains(err.Error(), "implement.md") {
		t.Errorf("error must not name a usable body; got: %v", err)
	}
}

// Load wires the loaded bodies onto Config so the stages can compose them.
func TestLoadSourcesPromptBodies(t *testing.T) {
	orig := promptsLoader
	t.Cleanup(func() { promptsLoader = orig })
	promptsLoader = func(string) (PromptBodies, error) {
		return PromptBodies{Implement: "I", Review: "V", Retro: "R"}, nil
	}
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Prompts.Implement != "I" || cfg.Prompts.Review != "V" || cfg.Prompts.Retro != "R" {
		t.Errorf("cfg.Prompts = %+v, want the loaded bodies", cfg.Prompts)
	}
}
