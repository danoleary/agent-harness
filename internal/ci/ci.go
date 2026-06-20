// Package ci is the host-side half of the review tool's post-PR step: it polls
// GitHub CI for the pushed branch and, on a failing check, drives a bounded
// auto-fix loop (diagnose + fix in the sandbox, re-run the gate, push, re-poll).
//
// Per ADR-0002 the harness owns all remote I/O, so every GitHub call here is
// host-side `gh` (through internal/proc, never the sandbox). The package keeps the
// effectful glue (the gh/proc calls, the sandbox session) behind small seams so
// the parsing, classification, and loop logic stay pure and unit-testable —
// mirroring internal/git's withRetry/runner split.
package ci

import "encoding/json"

// Check is one CI status check for a PR head, as `gh pr checks <branch> --json
// name,bucket,state,link` reports it. Bucket is gh's coarse classification
// (pass / fail / pending / skipping / cancel) — the field the harness keys its
// pass/fail/pending verdict off, rather than re-deriving it from State.
type Check struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	State  string `json:"state"`
	Link   string `json:"link"`
}

// ParseChecks decodes the JSON array `gh pr checks --json …` writes to stdout.
func ParseChecks(raw []byte) ([]Check, error) {
	var checks []Check
	if err := json.Unmarshal(raw, &checks); err != nil {
		return nil, err
	}
	return checks, nil
}
