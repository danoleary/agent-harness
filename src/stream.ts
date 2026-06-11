/**
 * Turn one line of claude's `--output-format stream-json` into a concise console
 * narration string, or `null` to skip it. The full raw stream is teed to the
 * per-run jsonl regardless; this is only the human-friendly summary shown when
 * the harness is not running `--verbose`. Never throws — a malformed line is
 * simply skipped so a single bad chunk can't kill the run.
 */
export function narrate(line: string): string | null {
	let event: unknown;
	try {
		event = JSON.parse(line);
	} catch {
		return null;
	}
	if (typeof event !== "object" || event === null) return null;

	const e = event as {
		type?: string;
		subtype?: string;
		is_error?: boolean;
		duration_ms?: number;
		message?: { content?: unknown[] };
	};

	if (e.type === "result") {
		const mark = e.is_error ? "✗" : "✓";
		const secs = typeof e.duration_ms === "number" ? ` (${Math.round(e.duration_ms / 1000)}s)` : "";
		return `${mark} session ${e.subtype ?? "ended"}${secs}`;
	}

	if (e.type === "assistant" && Array.isArray(e.message?.content)) {
		for (const block of e.message.content) {
			const b = block as { type?: string; name?: string };
			if (b.type === "tool_use" && b.name) {
				return `⚒ ${b.name}`;
			}
		}
	}

	return null;
}
