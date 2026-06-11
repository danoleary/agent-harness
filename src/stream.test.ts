import { expect, test } from "vitest";

import { narrate } from "./stream";

test("narrates a tool use as a concise action line", () => {
	const line = JSON.stringify({
		type: "assistant",
		message: { content: [{ type: "tool_use", name: "Bash", input: { command: "pnpm test" } }] },
	});

	const out = narrate(line);

	expect(out).toContain("Bash");
});

test("returns null for a malformed line instead of throwing", () => {
	expect(narrate("{not json")).toBeNull();
	expect(narrate("")).toBeNull();
});

test("returns null for events that aren't worth narrating", () => {
	const init = JSON.stringify({ type: "system", subtype: "init" });

	expect(narrate(init)).toBeNull();
});

test("narrates the final result event with success and duration", () => {
	const line = JSON.stringify({
		type: "result",
		subtype: "success",
		is_error: false,
		duration_ms: 12345,
	});

	const out = narrate(line);

	expect(out).toMatch(/result|done|✓/i);
});
