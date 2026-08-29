package config

import (
	"os"
	"path/filepath"
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

// A missing body file is not a hard error — the Stage still runs under the
// non-overridable envelope (ADR-0009 consequence 3). The body is the Consumer's
// quality concern; the contract is the harness's, and it lives in the envelope.
func TestLoadPromptsMissingBodyIsEmptyNotError(t *testing.T) {
	dir := t.TempDir() // no .agent-harness/prompts at all
	pb, err := LoadPrompts(dir)
	if err != nil {
		t.Fatalf("LoadPrompts should not error on missing bodies: %v", err)
	}
	if pb.Implement != "" || pb.Review != "" || pb.Retro != "" {
		t.Errorf("missing bodies should be empty, got %+v", pb)
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
