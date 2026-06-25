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
		premiseCheckSteer,
		"",
		t.Identifier + " is already claimed and moved to In Progress for you. Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O.",
		"",
		"If you hit problems with the harness or environment itself (setup friction, systemic gaps, missing patterns) during your session retrospective, do NOT file Linear issues. Instead append them to `/findings/out.json` as a JSON array of `{title, body, kind, key}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem). The harness reads this file after the session and files the issues for you, skipping any whose key already has an open issue. If you have no findings, leave the file untouched.",
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}

// premiseCheckSteer guards against the BEH-544 footgun: a ticket can be
// dispatched as live work after its fix already merged — sometimes under a
// *sibling* ticket, which the host-side own-key dispatch guard can't catch. The
// agent has the whole repo in front of it, so before planning it should confirm
// the ticket's premise still holds, and bail loudly rather than invent work if
// it doesn't.
const premiseCheckSteer = "Before you plan, verify the ticket's premise still holds: grep the worktree for the code symbols / file references it names (line/column refs in older tickets drift, and the cited work may already have landed — possibly under a different ticket). If the change has already been made — the dead code is gone, the behaviour is already present, the symbols no longer exist — do NOT fabricate a no-op change to look productive. Instead say so plainly: record \"already resolved — recommend close\" in your handoff commit (and stop there), so a wasted implementation session becomes an early signal."

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
		"If you hit problems with the harness or environment itself (setup friction, systemic gaps, missing patterns) during your session retrospective, do NOT file Linear issues. Instead append them to `/findings/out.json` as a JSON array of `{title, body, kind, key}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem). The harness reads this file after the session and files the issues for you, skipping any whose key already has an open issue. If you have no findings, leave the file untouched.",
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}

// FiledFinding is an already-filed harness-improvement finding class surfaced to
// a retrospective re-run so the session treats it as settled instead of
// re-deriving it (BEH-539). Key is the stable failure-class slug (may be empty
// for a title-only finding); Title is the human summary for the prompt list.
type FiledFinding struct {
	Key   string
	Title string
}

// BuildRetrospective builds the `-p` prompt for the sandboxed /retrospective
// session — the third, terminal tool. The retrospective studies the *sessions*,
// so it reads every prior transcript for the ticket plus the diff and writes
// harness/environment findings to the `/findings/out.json` dropbox. As with
// /tdd, the harness owns all remote I/O (ADR-0001/0002), so the prompt forbids
// Linear/MCP/push and any code change, and pins the always-write-`[]` rule the
// harness's ground-truth check depends on (an absent file means the step never
// ran).
func BuildRetrospective(t ticket.Ticket, slug string, filed []FiledFinding) string {
	logsPath := "agent-harness/logs/" + t.Identifier
	lines := []string{
		"/retrospective for " + t.Identifier + ". The worktree is at `.claude/worktrees/" + slug + "` on branch `feat/" + slug + "`.",
		"",
		"Read **every** prior transcript for this ticket under `" + logsPath + "/` (the `implementation-*.jsonl` and `review-*.jsonl` streams) plus the feature branch's diff against `main`. Study the sessions, not the feature — friction in the harness/environment, never the feature code (that was review's job).",
		"",
		"Write your findings to `/findings/out.json` as a JSON array of `{title, body, kind, key}` objects (kind is a free-form category; key is a stable, lowercase failure-class slug like `sandbox-playwright-missing-deps` used to dedup re-runs — pick the same key any session would for this class of problem, so the harness skips a finding whose key already has an open issue). **Always write the file**, even when you found nothing — write an empty array `[]` in that case. An absent file means the step never ran, so never end without writing it.",
		"",
		"This session is read-only and reaches no remote. Make NO code changes, do NOT commit or push, and do NOT touch Linear — do not call any `mcp__linear-server__*` tool. The harness reads `out.json` after the session and files each finding to Linear itself.",
	}
	if section := alreadyFiledSection(filed); section != "" {
		lines = append(lines, "", section)
	}
	lines = append(lines, "", bashQuirkSteer)
	return strings.Join(lines, "\n")
}

// alreadyFiledSection renders the re-run dedup context (BEH-539): the finding
// classes the harness has already filed, so the session treats them as settled
// and spends its budget on NEW friction instead of re-deriving them. Empty when
// there's nothing already filed — a first run reads exactly as it did before.
func alreadyFiledSection(filed []FiledFinding) string {
	if len(filed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("This ticket has been through the pipeline before — the harness has ALREADY filed Linear issues for the finding classes below, so treat them as SETTLED. Do NOT re-investigate, re-derive, or re-file them (the harness would skip them on key anyway); spend your budget only on NEW friction introduced since the last retrospective:")
	for _, f := range filed {
		b.WriteString("\n  - ")
		if f.Key != "" {
			b.WriteString(f.Key + " — ")
		}
		b.WriteString(f.Title)
	}
	return b.String()
}

// BuildCIFix builds the `-p` prompt for a sandboxed session that diagnoses and
// fixes a GitHub CI failure on the already-pushed PR (BEH-414). The PR is open
// and red; local gates passed but CI didn't (env/toolchain/flake/lockfile skew),
// so the harness fetched the failing job logs host-side and injects them here for
// the agent to reproduce + fix over the SAME existing worktree. As with review,
// the harness owns all remote I/O (ADR-0002): the agent commits the fix locally
// and stops — the harness re-pushes and re-polls CI. Each fix is its own commit
// (auditable trail; never a force-push over review history).
func BuildCIFix(t ticket.Ticket, slug, worktreePath, ciLogs string) string {
	lines := []string{
		"A GitHub CI check is failing on the open PR for " + t.Identifier + ". The worktree already exists at `" + worktreePath + "` on branch `feat/" + slug + "` — work in it; do NOT create a new worktree.",
		"",
		"The branch already passed the harness's local gate re-run, but CI on GitHub went red. The cause is usually a CI-vs-local difference (a different toolchain/env, a lockfile or native-binding skew, or a flaky spec) rather than something the local gate could catch.",
		"",
		"Here are the failing CI job logs the harness fetched for you (host-side, via `gh run view --log-failed`):",
		"",
		"```",
		ciLogs,
		"```",
		"",
		"Your job: read those logs, reproduce/diagnose the failure locally in the worktree where you can, and apply a fix as a NEW LOCAL commit on `feat/" + slug + "`. Re-run the relevant gate locally (the specific test/lint/typecheck/check that failed) to verify the fix before you stop. Keep the fix to its own commit — do NOT amend or force-push over the existing history.",
		"",
		"Ticket context (the intent — already fetched for you):",
		"",
		"# " + t.Identifier + ": " + t.Title,
		"",
		t.Description,
		"",
		"---",
		"",
		"Commit the fix LOCALLY and stop there. Do NOT push, do NOT run `gh`, do NOT open or comment on a PR. The harness owns all remote git I/O (ADR-0002): after you commit, it re-pushes the branch and re-polls CI itself.",
		"",
		"Do NOT touch Linear — do not call any `mcp__linear-server__*` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O (ADR-0001).",
		"",
		"Do NOT emit or file any harness-improvement findings, and do NOT write `/findings/out.json`. The retrospective tool owns findings — your only output is the fix commit.",
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
		"Do the diff-reading + seven-lens review FIRST, before running the memory-heavy gates (`pnpm run lint`/`build`/`test-storybook`/`typecheck`). This sandbox is memory-constrained and those gates routinely OOM-kill the session (exit 137); the harness re-runs every gate host-side anyway, so running them in-session mostly risks aborting the review before the lenses are applied. Whatever else happens, ALWAYS emit your `## Review:` report once the lenses are done — even though you don't push or open the PR, the harness keys off that report header to confirm the qualitative review actually ran, and otherwise flags the ticket as 'gates green but review incomplete'.",
		"",
		bashQuirkSteer,
	}
	return strings.Join(lines, "\n")
}
