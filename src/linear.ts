import type { Finding } from "./findings";
import type { Ticket } from "./types";

/** Sends a GraphQL operation and resolves the `data` payload (transport handles auth + errors). */
export type GraphQLTransport = (
	query: string,
	variables: Record<string, unknown>,
) => Promise<unknown>;

export interface LinearClient {
	/** Fetch a ticket by its human identifier (e.g. "BEH-362"). */
	fetchTicket(identifier: string): Promise<Ticket>;
	/** Move a ticket into its team's started "In Progress" state (claiming it). */
	moveToInProgress(identifier: string): Promise<void>;
	/** File a harness-improvement finding as a new issue referencing the worked ticket. */
	fileFinding(finding: Finding, opts: FileFindingOptions): Promise<CreatedIssue>;
}

export interface FileFindingOptions {
	/** UUID of the team to create the finding in (BeHerd). */
	teamId: string;
	/** Human identifier of the ticket whose session surfaced this (e.g. "BEH-362"). */
	relatedIdentifier: string;
}

export interface CreatedIssue {
	identifier: string;
	url: string;
}

const FETCH_TICKET = /* GraphQL */ `
	query Ticket($id: String!) {
		issue(id: $id) {
			identifier
			title
			description
			url
			priorityLabel
			team {
				id
			}
		}
	}
`;

const FETCH_ISSUE_STATES = /* GraphQL */ `
	query IssueStates($id: String!) {
		issue(id: $id) {
			id
			team {
				states {
					nodes {
						id
						name
						type
					}
				}
			}
		}
	}
`;

const MOVE_ISSUE = /* GraphQL */ `
	mutation MoveIssue($id: String!, $stateId: String!) {
		issueUpdate(id: $id, input: { stateId: $stateId }) {
			success
		}
	}
`;

interface IssueNode {
	identifier: string;
	title: string;
	description: string | null;
	url: string | null;
	priorityLabel: string | null;
	team: { id: string } | null;
}

const FILE_FINDING = /* GraphQL */ `
	mutation FileFinding($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue {
				identifier
				url
			}
		}
	}
`;

interface WorkflowState {
	id: string;
	name: string;
	type: string;
}

export function createLinearClient(transport: GraphQLTransport): LinearClient {
	return {
		async fetchTicket(identifier) {
			const data = (await transport(FETCH_TICKET, { id: identifier })) as {
				issue: IssueNode | null;
			};
			const issue = data.issue;
			if (!issue) {
				throw new Error(`Linear issue not found: ${identifier}`);
			}
			const ticket: Ticket = {
				identifier: issue.identifier,
				title: issue.title,
				description: issue.description ?? "",
			};
			if (issue.url) ticket.url = issue.url;
			if (issue.priorityLabel) ticket.priority = issue.priorityLabel;
			if (issue.team) ticket.teamId = issue.team.id;
			return ticket;
		},

		async moveToInProgress(identifier) {
			const data = (await transport(FETCH_ISSUE_STATES, { id: identifier })) as {
				issue: { id: string; team: { states: { nodes: WorkflowState[] } } } | null;
			};
			if (!data.issue) {
				throw new Error(`Linear issue not found: ${identifier}`);
			}
			const states = data.issue.team.states.nodes;
			const inProgress =
				states.find((s) => s.type === "started" && s.name === "In Progress") ??
				states.find((s) => s.type === "started");
			if (!inProgress) {
				throw new Error(`no started "In Progress" state for ${identifier}`);
			}
			await transport(MOVE_ISSUE, { id: data.issue.id, stateId: inProgress.id });
		},

		async fileFinding(finding, { teamId, relatedIdentifier }) {
			const kindLine = finding.kind ? `**Kind:** ${finding.kind}\n\n` : "";
			const description = `${kindLine}${finding.body}\n\n_Surfaced during ${relatedIdentifier} by the agent harness._`;
			const data = (await transport(FILE_FINDING, {
				input: { teamId, title: finding.title, description },
			})) as { issueCreate: { success: boolean; issue: CreatedIssue | null } };
			const issue = data.issueCreate.issue;
			if (!data.issueCreate.success || !issue) {
				throw new Error(`failed to file finding: ${finding.title}`);
			}
			return issue;
		},
	};
}
