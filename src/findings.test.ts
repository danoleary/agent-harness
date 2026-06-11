import { expect, test } from "vitest";

import { parseFindings } from "./findings";

test("returns no findings when the dropbox is empty", () => {
	const result = parseFindings("");

	expect(result.findings).toEqual([]);
	expect(result.error).toBeUndefined();
});

test("returns no findings when the dropbox is only whitespace", () => {
	const result = parseFindings("  \n\t ");

	expect(result.findings).toEqual([]);
	expect(result.error).toBeUndefined();
});

test("parses a valid array of findings", () => {
	const json = JSON.stringify([
		{ title: "tokens:build missing", body: "Storybook died until I ran it", kind: "setup" },
		{ title: "opaque vitest error", body: "bare file-list run crashes" },
	]);

	const result = parseFindings(json);

	expect(result.error).toBeUndefined();
	expect(result.findings).toHaveLength(2);
	expect(result.findings[0]).toEqual({
		title: "tokens:build missing",
		body: "Storybook died until I ran it",
		kind: "setup",
	});
	expect(result.findings[1]?.kind).toBeUndefined();
});

test("reports a soft error for malformed JSON without throwing", () => {
	const result = parseFindings("{not json");

	expect(result.findings).toEqual([]);
	expect(result.error).toMatch(/json/i);
});

test("reports a soft error when the JSON is not an array", () => {
	const result = parseFindings(JSON.stringify({ title: "x", body: "y" }));

	expect(result.findings).toEqual([]);
	expect(result.error).toMatch(/array/i);
});

test("skips entries missing required fields", () => {
	const json = JSON.stringify([
		{ title: "ok", body: "valid" },
		{ title: "no body" },
		"a string",
		{ body: "no title" },
	]);

	const result = parseFindings(json);

	expect(result.error).toBeUndefined();
	expect(result.findings).toEqual([{ title: "ok", body: "valid" }]);
});
