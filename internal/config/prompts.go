package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PromptBodies is the Consumer-supplied, per-Stage prompt body read from the
// bind-mounted checkout at `.agent-harness/prompts/{implement,review,retro}.md`
// (ADR-0009). Each body carries the Consumer's half of the prompt — which skill
// to invoke, project conventions, gate hints — which the harness composes inside
// its non-overridable contract envelope. A missing body is not an error: the
// Stage still runs under the envelope, and the body is the Consumer's quality
// concern, not part of the contract.
type PromptBodies struct {
	Implement string
	Review    string
	Retro     string
}

// LoadPrompts reads the per-Stage body files from
// `<checkoutPath>/.agent-harness/prompts/`. A missing file yields an empty body
// (the Stage runs under the envelope alone, ADR-0009); any other read error
// fails loud, since it signals a real filesystem problem rather than an
// intentionally-absent body.
func LoadPrompts(checkoutPath string) (PromptBodies, error) {
	dir := filepath.Join(ProjectDir(checkoutPath), "prompts")
	implement, err := readBody(dir, "implement")
	if err != nil {
		return PromptBodies{}, err
	}
	review, err := readBody(dir, "review")
	if err != nil {
		return PromptBodies{}, err
	}
	retro, err := readBody(dir, "retro")
	if err != nil {
		return PromptBodies{}, err
	}
	return PromptBodies{Implement: implement, Review: review, Retro: retro}, nil
}

func readBody(dir, name string) (string, error) {
	path := filepath.Join(dir, name+".md")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading prompt body %s: %w", path, err)
	}
	return string(raw), nil
}
