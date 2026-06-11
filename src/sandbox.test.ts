import { expect, test } from "vitest";

import {
	buildDockerRunArgs,
	DEFAULT_MOUNT_PATH,
	FINDINGS_MOUNT_PATH,
	type SandboxConfig,
} from "./sandbox";

/** docker `-v A:B` / `-e NAME` are positional pairs; pull the value following each flag occurrence. */
function valuesForFlag(args: string[], flag: string): string[] {
	const out: string[] = [];
	for (let i = 0; i < args.length; i++) {
		if (args[i] === flag) out.push(args[i + 1] ?? "");
	}
	return out;
}

const config: SandboxConfig = {
	image: "herd-agent-harness:latest",
	herdPath: "/Users/dan/herd",
	findingsDir: "/Users/dan/herd/agent-harness/logs/run-1/findings/BEH-362-tdd",
	pnpmStoreVolume: "herd-pnpm-store",
	prompt: "/tdd Work on BEH-362.",
};

test("runs claude with the prompt and the required print-mode flags", () => {
	const args = buildDockerRunArgs(config);

	// docker subcommand
	expect(args[0]).toBe("run");
	// the claude invocation lives at the tail, after the image
	const imageIdx = args.indexOf(config.image);
	const cmd = args.slice(imageIdx + 1);
	expect(cmd).toContain("claude");
	expect(cmd).toContain("-p");
	expect(cmd).toContain(config.prompt);
	expect(cmd).toContain("--dangerously-skip-permissions");
	expect(cmd.join(" ")).toContain("--output-format stream-json");
});

test("bind-mounts the herd checkout at the fixed path plus the pnpm store and findings dropbox", () => {
	const args = buildDockerRunArgs(config);
	const mounts = valuesForFlag(args, "-v");

	expect(mounts).toContain(`${config.herdPath}:${DEFAULT_MOUNT_PATH}`);
	expect(mounts).toContain(`${config.pnpmStoreVolume}:/pnpm-store`);
	expect(mounts).toContain(`${config.findingsDir}:${FINDINGS_MOUNT_PATH}`);
});

test("passes ANTHROPIC_API_KEY and GH_TOKEN by name but NEVER LINEAR_API_KEY", () => {
	const args = buildDockerRunArgs(config);
	const envNames = valuesForFlag(args, "-e");

	expect(envNames).toContain("ANTHROPIC_API_KEY");
	expect(envNames).toContain("GH_TOKEN");
	expect(envNames).not.toContain("LINEAR_API_KEY");
	// the boundary is absolute: no Linear secret anywhere in the argv
	expect(args.join(" ")).not.toMatch(/LINEAR/i);
});

test("names the container so the harness can kill it on timeout", () => {
	const args = buildDockerRunArgs({ ...config, containerName: "herd-harness-run1-tdd" });
	const names = valuesForFlag(args, "--name");

	expect(names).toContain("herd-harness-run1-tdd");
});

test("omits --name when no container name is given", () => {
	const args = buildDockerRunArgs(config);

	expect(args).not.toContain("--name");
});

test("passes secret values by name only — no value embedded in the argv", () => {
	const args = buildDockerRunArgs(config);

	// `-e NAME` (docker reads the value from the harness env), not `-e NAME=value`
	expect(args).toContain("ANTHROPIC_API_KEY");
	expect(args.join(" ")).not.toMatch(/ANTHROPIC_API_KEY=/);
});
