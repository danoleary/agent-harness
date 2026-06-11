/** Resolved harness configuration for a Phase-1 run-tdd invocation. */
export interface HarnessConfig {
	/** Host-only Linear key (ADR-0001) — never passed into the sandbox. */
	linearApiKey: string;
	/** Sandbox image tag. */
	image: string;
	/** Absolute host path to the herd checkout to bind-mount. */
	herdPath: string;
	/** Docker volume name for the persistent pnpm store. */
	pnpmStoreVolume: string;
	/** Wall-clock cap for the tdd session in ms. */
	tddTimeoutMs: number;
}

const DEFAULTS = {
	image: "herd-agent-harness:latest",
	pnpmStoreVolume: "herd-pnpm-store",
	tddTimeoutMs: 30 * 60 * 1000,
};

function requireEnv(env: NodeJS.ProcessEnv, key: string): string {
	const value = env[key];
	if (!value) {
		throw new Error(`missing required env var: ${key}`);
	}
	return value;
}

/**
 * Load harness config from the environment. The two sandbox secrets
 * (`ANTHROPIC_API_KEY`, `GH_TOKEN`) are validated for presence here but are not
 * returned — they flow into the container via docker's `-e NAME` reading the
 * harness's own inherited environment, so they never sit in our argv.
 */
export function loadConfig(env: NodeJS.ProcessEnv): HarnessConfig {
	requireEnv(env, "ANTHROPIC_API_KEY");
	requireEnv(env, "GH_TOKEN");

	const timeout = env.TDD_TIMEOUT_MS
		? Number.parseInt(env.TDD_TIMEOUT_MS, 10)
		: DEFAULTS.tddTimeoutMs;

	return {
		linearApiKey: requireEnv(env, "LINEAR_API_KEY"),
		herdPath: requireEnv(env, "HERD_PATH"),
		image: env.HARNESS_IMAGE ?? DEFAULTS.image,
		pnpmStoreVolume: env.PNPM_STORE_VOLUME ?? DEFAULTS.pnpmStoreVolume,
		tddTimeoutMs: Number.isFinite(timeout) && timeout > 0 ? timeout : DEFAULTS.tddTimeoutMs,
	};
}
