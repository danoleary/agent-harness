import { expect, test } from "vitest";

import { loadConfig } from "./config";

/** A complete env with every required var present. */
function fullEnv(overrides: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
	return {
		ANTHROPIC_API_KEY: "sk-ant-x",
		GH_TOKEN: "ghp_x",
		LINEAR_API_KEY: "lin_x",
		HERD_PATH: "/Users/dan/herd",
		...overrides,
	};
}

test("loads config from a complete environment with defaults applied", () => {
	const config = loadConfig(fullEnv());

	expect(config.linearApiKey).toBe("lin_x");
	expect(config.herdPath).toBe("/Users/dan/herd");
	expect(config.image).toBe("herd-agent-harness:latest");
	expect(config.pnpmStoreVolume).toBe("herd-pnpm-store");
	expect(config.tddTimeoutMs).toBe(30 * 60 * 1000);
});

test("honours optional overrides", () => {
	const config = loadConfig(
		fullEnv({
			HARNESS_IMAGE: "custom:tag",
			PNPM_STORE_VOLUME: "my-store",
			TDD_TIMEOUT_MS: "60000",
		}),
	);

	expect(config.image).toBe("custom:tag");
	expect(config.pnpmStoreVolume).toBe("my-store");
	expect(config.tddTimeoutMs).toBe(60000);
});

test.each(["ANTHROPIC_API_KEY", "GH_TOKEN", "LINEAR_API_KEY", "HERD_PATH"])(
	"throws when %s is missing",
	(key) => {
		const env = fullEnv();
		delete env[key];

		expect(() => loadConfig(env)).toThrow(new RegExp(key));
	},
);

test.each([
	["a non-numeric value", "abc"],
	["zero", "0"],
	["a negative value", "-5"],
	["an empty string", ""],
])("falls back to the default timeout for %s", (_label, value) => {
	const config = loadConfig(fullEnv({ TDD_TIMEOUT_MS: value }));

	expect(config.tddTimeoutMs).toBe(30 * 60 * 1000);
});
