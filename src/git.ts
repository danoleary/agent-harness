import { execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import { join } from "node:path";

import type { TddGroundTruth } from "./verify";

/** Host path of the worktree the tdd skill is told to create. */
export function worktreePath(herdPath: string, slug: string): string {
	return join(herdPath, ".claude", "worktrees", slug);
}

/**
 * Gather the ground truth a finished tdd session leaves behind, read from the
 * host's primary checkout. The sandbox commits into the shared `.git` (bind-
 * mounted), so the feature branch ref + objects are visible here without
 * touching the worktree itself — the harness never runs git inside a worktree
 * (whose `.git` pointer is container-relative), only against the main checkout.
 */
export function gatherTddGroundTruth(herdPath: string, slug: string): TddGroundTruth {
	const worktreeExists = existsSync(worktreePath(herdPath, slug));

	let commitsAhead = 0;
	try {
		const out = execFileSync(
			"git",
			["-C", herdPath, "rev-list", "--count", `origin/main..feat/${slug}`],
			{ encoding: "utf8" },
		);
		commitsAhead = Number.parseInt(out.trim(), 10) || 0;
	} catch {
		// branch doesn't exist / no upstream → treat as zero commits ahead
		commitsAhead = 0;
	}

	return { worktreeExists, commitsAhead };
}
