/** A Linear ticket, as the harness needs it (fetched host-side; see ADR-0001). */
export interface Ticket {
	/** Human identifier, e.g. "BEH-362". */
	identifier: string;
	title: string;
	/** Markdown description (includes the acceptance criteria). */
	description: string;
	/** Priority name, e.g. "Urgent" — for console narration. */
	priority?: string;
	url?: string;
	/** UUID of the ticket's team — findings are filed back into the same team. */
	teamId?: string;
}
