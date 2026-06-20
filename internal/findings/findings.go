// Package findings parses the harness-improvement findings dropbox a session
// drops into `/findings/out.json`.
package findings

import (
	"encoding/json"
	"strings"
)

// Finding is a harness-improvement finding a session dropped into the dropbox.
type Finding struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// Kind is a free-form category the session tagged it with (e.g. "setup").
	Kind string `json:"kind,omitempty"`
	// LabelIDs are any per-finding Linear label UUIDs to apply on top of the
	// always-present agent-harness label (BEH-409). The dropbox doesn't surface
	// these today; the field is the seam so a future kind-derived label is added
	// alongside agent-harness, not in place of it.
	LabelIDs []string `json:"-"`
}

// Parsed is the result of reading the findings dropbox: the valid findings, plus
// a soft parse error if any.
type Parsed struct {
	Findings []Finding
	// Error is set when the dropbox was present but unreadable — logged, never
	// thrown (ADR-0001: degraded, not broken). Empty when there was no error.
	Error string
}

// Parse reads the contents of the findings dropbox (`/findings/out.json`). The
// common case is an empty/absent file → no findings filed. A malformed file is
// reported as a soft error rather than failing, so one bad session can't crash
// the run.
func Parse(text string) Parsed {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Parsed{Findings: []Finding{}}
	}

	var top any
	if err := json.Unmarshal([]byte(trimmed), &top); err != nil {
		return Parsed{Findings: []Finding{}, Error: "dropbox is not valid JSON: " + err.Error()}
	}

	arr, ok := top.([]any)
	if !ok {
		return Parsed{Findings: []Finding{}, Error: "dropbox JSON is not an array of findings"}
	}

	out := []Finding{}
	for _, entry := range arr {
		obj, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		title, titleOK := obj["title"].(string)
		body, bodyOK := obj["body"].(string)
		if !titleOK || !bodyOK {
			continue
		}
		f := Finding{Title: title, Body: body}
		if kind, kindOK := obj["kind"].(string); kindOK {
			f.Kind = kind
		}
		out = append(out, f)
	}

	return Parsed{Findings: out}
}
