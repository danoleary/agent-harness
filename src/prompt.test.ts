import { expect, test } from "vitest";

import { buildTddPrompt } from "./prompt";
import type { Ticket } from "./types";

const ticket: Ticket = {
	identifier: "BEH-362",
	title: "Agent harness Phase 1: run-tdd CLI",
	description:
		"## What to build\n\nA minimal CLI.\n\n## Acceptance criteria\n\n- [ ] fetch the ticket",
	priority: "Urgent",
};

test("invokes the /tdd skill on the given ticket and slug", () => {
	const prompt = buildTddPrompt({ ticket, slug: "beh-362" });

	expect(prompt).toContain("/tdd");
	expect(prompt).toContain("BEH-362");
	expect(prompt).toContain("slug `beh-362`");
});

test("injects the ticket title and description so the skill needn't fetch them", () => {
	const prompt = buildTddPrompt({ ticket, slug: "beh-362" });

	expect(prompt).toContain(ticket.title);
	expect(prompt).toContain("Acceptance criteria");
	expect(prompt).toContain("fetch the ticket");
});

test("steers the skill off Linear — already claimed, harness owns Linear I/O", () => {
	const prompt = buildTddPrompt({ ticket, slug: "beh-362" });

	expect(prompt).toMatch(/already.*(claimed|In Progress)/i);
	expect(prompt).toMatch(/(do not|don't).*Linear/i);
});

test("redirects harness-improvement findings to the dropbox, not Linear", () => {
	const prompt = buildTddPrompt({ ticket, slug: "beh-362" });

	expect(prompt).toContain("/findings/out.json");
	expect(prompt).toMatch(/title.*body.*kind/);
});
