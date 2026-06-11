import { expect, test } from "vitest";

import { makeRunId } from "./log";

test("formats a zero-padded, filesystem-safe run id from a date", () => {
	const id = makeRunId(new Date(2026, 5, 11, 14, 8, 5));

	expect(id).toBe("20260611-140805");
});

test("sorts lexicographically in chronological order", () => {
	const earlier = makeRunId(new Date(2026, 0, 1, 0, 0, 0));
	const later = makeRunId(new Date(2026, 11, 31, 23, 59, 59));

	expect(earlier < later).toBe(true);
	expect(earlier).toBe("20260101-000000");
	expect(later).toBe("20261231-235959");
});
