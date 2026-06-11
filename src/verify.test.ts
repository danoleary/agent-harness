import { expect, test } from "vitest";

import { verifyTdd } from "./verify";

test("passes when the worktree exists and the branch is ahead of main", () => {
	const result = verifyTdd({ worktreeExists: true, commitsAhead: 1 });

	expect(result.ok).toBe(true);
});

test("fails when the worktree was never created", () => {
	const result = verifyTdd({ worktreeExists: false, commitsAhead: 0 });

	expect(result.ok).toBe(false);
	expect(result.reason).toMatch(/worktree/i);
});

test("fails when the branch has no commit ahead of main", () => {
	const result = verifyTdd({ worktreeExists: true, commitsAhead: 0 });

	expect(result.ok).toBe(false);
	expect(result.reason).toMatch(/commit/i);
});
