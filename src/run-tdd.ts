import { execFileSync, spawn } from "node:child_process";
import { mkdirSync, readFileSync } from "node:fs";
import { createInterface } from "node:readline";
import { join } from "node:path";

import { loadConfig } from "./config";
import { gatherTddGroundTruth } from "./git";
import { createLinearClient } from "./linear";
import { createLinearTransport } from "./linear-transport";
import { parseFindings } from "./findings";
import { buildTddPrompt } from "./prompt";
import { buildDockerRunArgs } from "./sandbox";
import { createLogger, makeRunId } from "./log";
import { narrate } from "./stream";
import { verifyTdd } from "./verify";

interface CliArgs {
	identifier: string;
	dryRun: boolean;
	verbose: boolean;
}

const TICKET_RE = /^[A-Z]+-\d+$/;

function parseArgs(argv: string[]): CliArgs {
	let identifier = "";
	let dryRun = false;
	let verbose = false;
	for (const arg of argv) {
		if (arg === "--dry-run") dryRun = true;
		else if (arg === "--verbose") verbose = true;
		else if (!arg.startsWith("-") && !identifier) identifier = arg.toUpperCase();
	}
	if (!TICKET_RE.test(identifier)) {
		throw new Error(`usage: run-tdd <TICKET-ID> [--dry-run] [--verbose]  (got: "${identifier}")`);
	}
	return { identifier, dryRun, verbose };
}

/** Run the sandboxed tdd session, teeing the stream-json transcript to disk and narrating to the console. */
async function runSandbox(
	dockerArgs: string[],
	opts: {
		containerName: string;
		transcriptFile: string;
		timeoutMs: number;
		verbose: boolean;
		tee: (file: string, raw: string) => void;
		event: (msg: string) => void;
	},
): Promise<number> {
	return new Promise((resolve) => {
		const child = spawn("docker", dockerArgs, { stdio: ["ignore", "pipe", "pipe"] });

		const killTimer = setTimeout(() => {
			opts.event(`tdd ✗ wall-clock cap hit — killing ${opts.containerName}`);
			try {
				execFileSync("docker", ["kill", opts.containerName]);
			} catch {
				/* container may already be gone */
			}
		}, opts.timeoutMs);

		const rl = createInterface({ input: child.stdout });
		rl.on("line", (line) => {
			opts.tee(opts.transcriptFile, line);
			if (opts.verbose) {
				process.stdout.write(`${line}\n`);
			} else {
				const narration = narrate(line);
				if (narration) opts.event(narration);
			}
		});

		// Line-buffer stderr too so the forensic transcript stays one record per
		// line, rather than however the OS happened to chunk the pipe.
		const errRl = createInterface({ input: child.stderr });
		errRl.on("line", (line) => {
			opts.tee(opts.transcriptFile, line);
		});

		child.on("close", (code) => {
			clearTimeout(killTimer);
			resolve(code ?? 1);
		});
		child.on("error", (err) => {
			clearTimeout(killTimer);
			opts.event(`tdd ✗ failed to launch docker: ${err.message}`);
			resolve(1);
		});
	});
}

async function main(): Promise<number> {
	const args = parseArgs(process.argv.slice(2));
	const config = loadConfig(process.env);

	const runId = makeRunId(new Date());
	const log = createLogger(join(config.herdPath, "agent-harness", "logs"), runId);
	const slug = args.identifier.toLowerCase();

	log.event(`run ${runId} — tdd ${args.identifier}${args.dryRun ? " (dry-run)" : ""}`);

	const linear = createLinearClient(createLinearTransport(config.linearApiKey));

	const ticket = await linear.fetchTicket(args.identifier);
	log.event(`fetched ${ticket.identifier} (${ticket.priority ?? "No priority"}) — ${ticket.title}`);

	const prompt = buildTddPrompt({ ticket, slug });
	const findingsDir = join(log.runDir, "findings", `${args.identifier}-tdd`);
	mkdirSync(findingsDir, { recursive: true });

	// runId is second-resolution; include the pid so two runs started in the same
	// second still get distinct container names (and distinct `docker kill` targets).
	const containerName = `herd-harness-${runId}-${process.pid}-tdd`;
	const dockerArgs = buildDockerRunArgs({
		image: config.image,
		herdPath: config.herdPath,
		findingsDir,
		pnpmStoreVolume: config.pnpmStoreVolume,
		prompt,
		containerName,
	});

	if (args.dryRun) {
		log.event("dry-run — not claiming the ticket, not launching the container");
		process.stdout.write(
			`\n--- prompt ---\n${prompt}\n\n--- docker command ---\ndocker ${dockerArgs.join(" ")}\n`,
		);
		return 0;
	}

	await linear.moveToInProgress(args.identifier);
	log.event(`claimed ${args.identifier} → In Progress`);

	const transcriptFile = `${args.identifier}-tdd.jsonl`;
	log.event(`launching sandbox (cap ${Math.round(config.tddTimeoutMs / 60000)} min)`);
	const exitCode = await runSandbox(dockerArgs, {
		containerName,
		transcriptFile,
		timeoutMs: config.tddTimeoutMs,
		verbose: args.verbose,
		tee: log.teeLine,
		event: log.event,
	});
	log.event(`session exited (code ${exitCode}) — transcript at logs/${runId}/${transcriptFile}`);

	// Ground truth, never self-report.
	const truth = gatherTddGroundTruth(config.herdPath, slug);
	const result = verifyTdd(truth);
	log.event(
		result.ok
			? `tdd ✓ ${result.reason} (${truth.commitsAhead} commit${truth.commitsAhead === 1 ? "" : "s"} ahead)`
			: `tdd ✗ ${result.reason}`,
	);

	// File any harness-improvement findings the session dropped (after every session, per ADR-0001).
	await fileFindings(findingsDir, ticket.teamId, args.identifier, linear, log);

	return result.ok ? 0 : 1;
}

async function fileFindings(
	findingsDir: string,
	teamId: string | undefined,
	relatedIdentifier: string,
	linear: ReturnType<typeof createLinearClient>,
	log: ReturnType<typeof createLogger>,
): Promise<void> {
	let text = "";
	try {
		text = readFileSync(join(findingsDir, "out.json"), "utf8");
	} catch {
		return; // no dropbox file → nothing to file (the common, friction-free case)
	}

	const { findings, error } = parseFindings(text);
	if (error) {
		log.event(`findings ✗ dropbox unreadable: ${error}`);
		return;
	}
	if (findings.length === 0) return;
	if (!teamId) {
		log.event(
			`findings ✗ ${findings.length} dropped but no team id resolved for ${relatedIdentifier}`,
		);
		return;
	}

	for (const finding of findings) {
		try {
			const created = await linear.fileFinding(finding, { teamId, relatedIdentifier });
			log.event(`finding filed: ${created.identifier} — ${finding.title}`);
		} catch (err) {
			log.event(`finding ✗ failed to file "${finding.title}": ${(err as Error).message}`);
		}
	}
}

main()
	.then((code) => process.exit(code))
	.catch((err) => {
		process.stderr.write(`${(err as Error).stack ?? err}\n`);
		process.exit(1);
	});
