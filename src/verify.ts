/** Ground-truth state of a finished tdd session, gathered from git (never self-reported). */
export interface TddGroundTruth {
	/** Whether `.claude/worktrees/<slug>` exists. */
	worktreeExists: boolean;
	/** Commits on `feat/<slug>` ahead of the merge-base with `origin/main`. */
	commitsAhead: number;
}

/** Outcome of checking a tdd session against ground truth. */
export interface VerifyResult {
	ok: boolean;
	reason: string;
}

/**
 * Decide whether a tdd session really did its job. Success requires both a
 * worktree and at least one commit on the feature branch — the agent's own
 * "I'm done" is never authoritative (DESIGN.md: success is ground-truth).
 */
export function verifyTdd(truth: TddGroundTruth): VerifyResult {
	if (!truth.worktreeExists) {
		return { ok: false, reason: "worktree was not created" };
	}
	if (truth.commitsAhead < 1) {
		return { ok: false, reason: "no handoff commit ahead of origin/main" };
	}
	return { ok: true, reason: "worktree present and branch is ahead of main" };
}
