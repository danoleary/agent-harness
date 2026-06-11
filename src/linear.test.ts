import { expect, test } from "vitest";

import { createLinearClient, type GraphQLTransport } from "./linear";

test("fetchTicket parses the issue into a Ticket", async () => {
	const transport: GraphQLTransport = async () => ({
		issue: {
			identifier: "BEH-362",
			title: "Agent harness Phase 1",
			description: "## What to build\n\nA CLI.",
			url: "https://linear.app/beherd/issue/BEH-362",
			priorityLabel: "Urgent",
			team: { id: "team-uuid" },
		},
	});
	const linear = createLinearClient(transport);

	const ticket = await linear.fetchTicket("BEH-362");

	expect(ticket).toEqual({
		identifier: "BEH-362",
		title: "Agent harness Phase 1",
		description: "## What to build\n\nA CLI.",
		url: "https://linear.app/beherd/issue/BEH-362",
		priority: "Urgent",
		teamId: "team-uuid",
	});
});

test("fetchTicket throws when the issue does not exist", async () => {
	const linear = createLinearClient(async () => ({ issue: null }));

	await expect(linear.fetchTicket("BEH-999")).rejects.toThrow(/BEH-999/);
});

test("moveToInProgress updates the issue to the team's started In Progress state", async () => {
	const calls: { query: string; variables: Record<string, unknown> }[] = [];
	const transport: GraphQLTransport = async (query, variables) => {
		calls.push({ query, variables });
		if (query.includes("states")) {
			return {
				issue: {
					id: "issue-uuid",
					team: {
						states: {
							nodes: [
								{ id: "state-todo", name: "Todo", type: "unstarted" },
								{ id: "state-progress", name: "In Progress", type: "started" },
								{ id: "state-review", name: "In Review", type: "started" },
							],
						},
					},
				},
			};
		}
		return { issueUpdate: { success: true } };
	};
	const linear = createLinearClient(transport);

	await linear.moveToInProgress("BEH-362");

	const update = calls.find((c) => c.query.includes("issueUpdate"));
	expect(update).toBeDefined();
	expect(update?.variables.id).toBe("issue-uuid");
	expect(update?.variables.stateId).toBe("state-progress");
});

test("fileFinding creates an issue in the team that references the worked ticket", async () => {
	let captured: Record<string, unknown> | undefined;
	const transport: GraphQLTransport = async (_query, variables) => {
		captured = variables;
		return {
			issueCreate: { success: true, issue: { identifier: "BEH-400", url: "https://x/BEH-400" } },
		};
	};
	const linear = createLinearClient(transport);

	const created = await linear.fileFinding(
		{ title: "tokens:build missing", body: "Storybook died", kind: "setup" },
		{ teamId: "team-uuid", relatedIdentifier: "BEH-362" },
	);

	expect(created.identifier).toBe("BEH-400");
	const input = captured?.input as Record<string, unknown>;
	expect(input.teamId).toBe("team-uuid");
	expect(input.title).toBe("tokens:build missing");
	expect(String(input.description)).toContain("Storybook died");
	expect(String(input.description)).toContain("BEH-362");
});
