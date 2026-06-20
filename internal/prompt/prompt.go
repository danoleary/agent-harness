// Package prompt builds the `-p` prompts for the sandboxed Claude sessions the
// harness drives (/tdd for implementation, /review-worktree for review,
// /retrospective for the terminal findings pass).
package prompt

import (
	"strings"

	"github.com/beherd/agent-harness/internal/ticket"
)

// bashQuirkSteer warns the sandboxed agent off the command pattern that the
// pinned Claude CLI's bash wrapper intermittently mangles (BEH-401): chaining a
// pipe into `head`/`tail` together with a command substitution like `cd "$(...)"`
// in a single Bash call gets misparsed into opaque errors (`head: invalid number
// of bytes`, `cd: too many arguments`) that read like the agent's own bug. The
// harness can't patch the upstream CLI, so it steers around the trigger instead.
const bashQuirkSteer = "Sandbox bash quirk (BEH-401): the bundled Claude CLI intermittently mangles a single Bash call that BOTH pipes into `head`/`tail` AND uses a command substitution like `cd \"$(...)\"`, producing opaque errors (`head: invalid number of bytes: 'set -euo pipefail; ...'`, `cd: too many arguments`) that look like a bug in your command but are an environment quirk. Work around it: run one command per Bash call, prefer absolute paths over `cd \"$(...)\"`, and don't tack `| head -n N` onto a compound command — if a check misfires this way, retrying it verbatim won't help, so split it up."

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
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}

// BuildTddResume builds the `-p` prompt for a *retry* of the tdd session after a
// usage-policy refusal (BEH-389). The first attempt already created the worktree
// and left its work uncommitted on disk (the real-path bind mount survives the
// refusal), so this prompt steers the skill to RESUME that existing worktree and
// commit the surviving work rather than recreate it — re-running the skill's
// worktree-creation step would fail on the already-existing `feat/<slug>` branch.
// Every other steer (Linear off, findings to the dropbox) is unchanged.
func BuildTddResume(t ticket.Ticket, slug, worktreePath string) string {
	lines := []string{
		"/tdd Continue work on " + t.Identifier + ".",
		"",
		"IMPORTANT: a previous attempt was interrupted (a usage-policy refusal), but its work survives. The worktree ALREADY EXISTS at `" + worktreePath + "` on branch `feat/" + slug + "`, and it likely holds uncommitted changes from that attempt. Do NOT create a new worktree and do NOT run `new-worktree.sh` — that would fail on the already-existing branch. `cd` into the existing worktree, inspect what's there with `git status`/`git diff`, finish anything incomplete, run the gates, and commit the handoff. Recovering and committing that surviving diff is the whole point of this retry.",
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
		"",
		bashQuirkSteer,
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
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}

// BuildReview builds the `-p` prompt for the sandboxed /review-worktree session
// (the second tool). The implementation slice already left a worktree + handoff
// commit; review reads that diff *cold* and applies fixes as a local commit. The
// harness (host-side, holding GH_TOKEN) independently re-runs the gates and ships
// it — so the prompt steers /review-worktree off every remote/exit step the skill
// would otherwise take (push, `gh`, Linear, findings) and pins it to a local
// commit only. See DESIGN.md "Build order" + ADR-0002.
func BuildReview(t ticket.Ticket, slug, worktreePath string) string {
	lines := []string{
		"/review-worktree " + worktreePath,
		"",
		"You are reviewing the worktree on branch `feat/" + slug + "` for " + t.Identifier + ".",
		"",
		"Ticket context (the intent to review against — already fetched for you):",
		"",
		"# " + t.Identifier + ": " + t.Title,
		"",
		t.Description,
		"",
		"---",
		"",
		"This is a COLD review. Reconstruct the intent from the branch, this ticket, and the diff ONLY. Do NOT read the implementation session's transcript (under `agent-harness/logs/" + t.Identifier + "/`) — coldness is the point; an independent reviewer must not be anchored to the implementer's framing.",
		"",
		"Apply your review fixes as a LOCAL commit on `feat/" + slug + "` and stop there. Do NOT push, do NOT run `gh`, do NOT open or raise a PR. The harness owns all remote git I/O (ADR-0002): it independently re-runs the quality gates host-side and, only if they pass, pushes the branch and opens the PR itself.",
		"",
		"Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O (ADR-0001).",
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool, running last over every transcript, owns findings now — this is a deliberate change from the skill's default. Your only outputs are review fixes committed locally.",
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}
