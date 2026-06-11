import type { Ticket } from "./types";

export interface BuildTddPromptOptions {
	ticket: Ticket;
	/** Deterministic worktree slug the harness dictates, e.g. "beh-362". */
	slug: string;
}

/**
 * Build the `-p` prompt for the sandboxed `/tdd` session. The harness has
 * already claimed the ticket and owns all Linear I/O (ADR-0001), so the prompt
 * steers the skill off `mcp__linear-server__*`, injects the ticket context the
 * skill would otherwise fetch, and points harness-improvement findings at the
 * mounted dropbox instead of Linear.
 */
export function buildTddPrompt({ ticket, slug }: BuildTddPromptOptions): string {
	return [
		`/tdd Work on ${ticket.identifier}. Create the worktree with slug \`${slug}\`.`,
		"",
		`Ticket context (already fetched for you — do not look it up):`,
		"",
		`# ${ticket.identifier}: ${ticket.title}`,
		"",
		ticket.description,
		"",
		"---",
		"",
		`${ticket.identifier} is already claimed and moved to In Progress for you. Do NOT touch Linear — do not call any \`mcp__linear-server__*\` tool, do not move the ticket, do not open or comment on issues. The harness owns all Linear I/O.`,
		"",
		`If you hit problems with the harness or environment itself (setup friction, systemic gaps, missing patterns) during your session retrospective, do NOT file Linear issues. Instead append them to \`/findings/out.json\` as a JSON array of \`{title, body, kind}\` objects (kind is a free-form category). The harness reads this file after the session and files the issues for you. If you have no findings, leave the file untouched.`,
	].join("\n");
}
