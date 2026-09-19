package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PromptBodies is the Consumer-supplied, per-Stage prompt body read from the
// bind-mounted checkout at `.agent-harness/prompts/{implement,review,retro}.md`
// (ADR-0009). Each body carries the Consumer's half of the prompt — which skill
// to invoke, project conventions, gate hints — which the harness composes inside
// its non-overridable contract envelope.
//
// All three are required. The harness declares no skill of its own for any Stage,
// so the body is the only thing that can name one.
type PromptBodies struct {
	Implement string
	Review    string
	Retro     string
}

// LoadPrompts reads the per-Stage body files from
// `<checkoutPath>/.agent-harness/prompts/`.
//
// A missing or blank body is a hard error, because the harness has no default
// skill to fall back on: the Stage would run under the envelope alone, with
// nothing to invoke. That failure is invisible from the outside — the container
// starts, the session reports success, the claim and the ticket are spent — so it
// must surface at config load, before the first container, rather than as an
// empty worktree an hour later (ADR-0009, amended).
//
// Every unusable body is reported in ONE error. A project adopting the harness
// typically has none of the three, and finding that out one failed run at a time
// is three cycles of the same discovery.
func LoadPrompts(checkoutPath string) (PromptBodies, error) {
	dir := filepath.Join(ProjectDir(checkoutPath), "prompts")

	var pb PromptBodies
	stages := []struct {
		name string
		dest *string
	}{
		{"implement", &pb.Implement},
		{"review", &pb.Review},
		{"retro", &pb.Retro},
	}

	var unusable []string
	for _, st := range stages {
		body, err := readBody(dir, st.name)
		if err != nil {
			return PromptBodies{}, err
		}
		// Blank and absent collapse to the same verdict on purpose: `touch
		// implement.md` leaves the Stage exactly as skill-less as no file at all.
		if strings.TrimSpace(body) == "" {
			unusable = append(unusable, filepath.Join(dir, st.name+".md"))
			continue
		}
		*st.dest = body
	}

	if len(unusable) > 0 {
		return PromptBodies{}, fmt.Errorf(
			"missing or blank prompt body: %s — the harness declares no skill of its own, so each stage's body must name the skill it invokes (see docs/CONSUMER.md)",
			strings.Join(unusable, ", "),
		)
	}
	return pb, nil
}

// readBody reads one body file. An absent file yields an empty string rather than
// an error, so the caller judges absent and blank by one rule.
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
