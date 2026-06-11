/** Fixed container path the herd checkout is bind-mounted at in every sandbox (DESIGN.md). */
export const DEFAULT_MOUNT_PATH = "/workspace/herd";
/** Fixed container path the findings dropbox is mounted at. */
export const FINDINGS_MOUNT_PATH = "/findings";
/** Fixed container path the persistent pnpm content-addressed store is mounted at. */
export const PNPM_STORE_MOUNT_PATH = "/pnpm-store";

/**
 * The only secrets that ever cross the sandbox boundary (ADR-0001). LINEAR_API_KEY
 * is deliberately absent — Linear access never enters the container.
 */
export const SANDBOX_SECRET_ENV = ["ANTHROPIC_API_KEY", "GH_TOKEN"] as const;

export interface SandboxConfig {
	/** Sandbox image tag. */
	image: string;
	/** Host path to the herd checkout to bind-mount. */
	herdPath: string;
	/** Host path to this session's findings dropbox dir, mounted at /findings. */
	findingsDir: string;
	/** Docker volume name for the persistent pnpm store. */
	pnpmStoreVolume: string;
	/** The `-p` prompt to hand to claude. */
	prompt: string;
	/** Container path the checkout is mounted at; defaults to {@link DEFAULT_MOUNT_PATH}. */
	mountPath?: string;
	/** Optional `--name` so the harness can `docker kill` the container on timeout. */
	containerName?: string;
}

/**
 * Build the argv (everything after `docker`) to run one sandboxed `claude`
 * session. Secrets are passed by name only (`-e NAME`, no value) so they are
 * read from the harness's own environment at spawn time and never appear in the
 * process table. The herd checkout is mounted at a single fixed path so a
 * worktree's container-relative `.git` pointer resolves identically every run.
 */
export function buildDockerRunArgs(config: SandboxConfig): string[] {
	const mountPath = config.mountPath ?? DEFAULT_MOUNT_PATH;

	const args = ["run", "--rm", "--init"];

	if (config.containerName) {
		args.push("--name", config.containerName);
	}

	for (const name of SANDBOX_SECRET_ENV) {
		args.push("-e", name);
	}

	args.push(
		"-v",
		`${config.herdPath}:${mountPath}`,
		"-v",
		`${config.pnpmStoreVolume}:${PNPM_STORE_MOUNT_PATH}`,
		"-v",
		`${config.findingsDir}:${FINDINGS_MOUNT_PATH}`,
		"-w",
		mountPath,
		config.image,
		"claude",
		"-p",
		config.prompt,
		"--dangerously-skip-permissions",
		"--output-format",
		"stream-json",
		"--verbose",
	);

	return args;
}
