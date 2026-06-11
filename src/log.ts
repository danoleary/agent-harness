import { appendFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";

export interface Logger {
	/** Absolute path of this run's log directory (`logs/<run-id>/`). */
	runDir: string;
	/** Emit one concise narration line to the console and mirror it to `run.jsonl`. */
	event(message: string): void;
	/** Append a raw transcript line (e.g. a claude stream-json chunk) to a per-run file. */
	teeLine(file: string, raw: string): void;
}

/** Build a sortable, filesystem-safe run id from a timestamp, e.g. `20260611-140815`. */
export function makeRunId(now: Date): string {
	const pad = (n: number) => String(n).padStart(2, "0");
	return (
		`${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}` +
		`-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`
	);
}

export function createLogger(logsRoot: string, runId: string): Logger {
	const runDir = join(logsRoot, runId);
	mkdirSync(runDir, { recursive: true });

	return {
		runDir,
		event(message) {
			const ts = new Date().toISOString();
			console.log(`${ts}  ${message}`);
			appendFileSync(join(runDir, "run.jsonl"), `${JSON.stringify({ ts, message })}\n`);
		},
		teeLine(file, raw) {
			appendFileSync(join(runDir, file), raw.endsWith("\n") ? raw : `${raw}\n`);
		},
	};
}
