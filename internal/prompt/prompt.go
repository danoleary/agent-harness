// Package prompt builds the `-p` prompt for the sandboxed /tdd session.
package prompt

import (
	"strings"

	"github.com/beherd/agent-harness/internal/ticket"
)

// BuildTdd builds the `-p` prompt for the sandboxed /tdd session. The harness
// has already claimed the ticket and owns all Linear I/O (ADR-0001), so the
// prompt steers the skill off `mcp__linear-server__*`, injects the ticket
// context the skill would otherwise fetch, and points harness-improvement
// findings at the mounted dropbox instead of Linear.
func BuildTdd(t ticket.Ticket, slug string) string {
	lines := []string{
		"/tdd Work on " + t.Identifier + ". Create the worktree with slug `" + slug + "`.",
		"",
		"Ticket context (already fetched for you — do not look it up):",
		"",
		"# " + t.Identifier + ": " + t.Title,
		"",
		t.Description,
		"",
		"---",
		"",
		t.Identifier + " is already claimed and moved to In Progress for you. Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O.",
		"",
		"If you hit problems with the harness or environment itself (setup friction, systemic gaps, missing patterns) during your session retrospective, do NOT file Linear issues. Instead append them to `/findings/out.json` as a JSON array of `{title, body, kind}` objects (kind is a free-form category). The harness reads this file after the session and files the issues for you. If you have no findings, leave the file untouched.",
	}
	return strings.Join(lines, "\n")
}

// BuildRetrospective builds the `-p` prompt for the sandboxed /retrospective
// session — the third, terminal tool. The retrospective studies the *sessions*,
// so it reads every prior transcript for the ticket plus the diff and writes
// harness/environment findings to the `/findings/out.json` dropbox. As with
// /tdd, the harness owns all remote I/O (ADR-0001/0002), so the prompt forbids
// Linear/MCP/push and any code change, and pins the always-write-`[]` rule the
// harness's ground-truth check depends on (an absent file means the step never
// ran).
func BuildRetrospective(t ticket.Ticket, slug string) string {
	logsPath := "agent-harness/logs/" + t.Identifier
	lines := []string{
		"/retrospective for " + t.Identifier + ". The worktree is at `.claude/worktrees/" + slug + "` on branch `feat/" + slug + "`.",
		"",
		"Read **every** prior transcript for this ticket under `" + logsPath + "/` (the `implementation-*.jsonl` and `review-*.jsonl` streams) plus the feature branch's diff against `main`. Study the sessions, not the feature — friction in the harness/environment, never the feature code (that was review's job).",
		"",
		"Write your findings to `/findings/out.json` as a JSON array of `{title, body, kind}` objects (kind is a free-form category). **Always write the file**, even when you found nothing — write an empty array `[]` in that case. An absent file means the step never ran, so never end without writing it.",
		"",
		"This session is read-only and reaches no remote. Make NO code changes, do NOT commit or push, and do NOT touch Linear — do not call any `mcp__linear-server__*` tool. The harness reads `out.json` after the session and files each finding to Linear itself.",
	}
	return strings.Join(lines, "\n")
}
