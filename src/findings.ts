/** A harness-improvement finding a session dropped into `/findings/out.json`. */
export interface Finding {
	title: string;
	body: string;
	/** Free-form category the session tagged it with (e.g. "setup", "tooling"). */
	kind?: string;
}

/** Result of reading the findings dropbox: the valid findings, plus a soft parse error if any. */
export interface ParsedFindings {
	findings: Finding[];
	/** Set when the dropbox was present but unreadable — logged, never thrown (ADR-0001: degraded, not broken). */
	error?: string;
}

/**
 * Parse the contents of the findings dropbox (`/findings/out.json`). The common
 * case is an empty/absent file → no findings filed. A malformed file is reported
 * as a soft error rather than throwing, so one bad session can't crash the run.
 */
export function parseFindings(text: string): ParsedFindings {
	const trimmed = text.trim();
	if (trimmed === "") {
		return { findings: [] };
	}

	let raw: unknown;
	try {
		raw = JSON.parse(trimmed);
	} catch (err) {
		return { findings: [], error: `dropbox is not valid JSON: ${(err as Error).message}` };
	}

	if (!Array.isArray(raw)) {
		return { findings: [], error: "dropbox JSON is not an array of findings" };
	}

	const findings: Finding[] = [];
	for (const entry of raw) {
		if (
			typeof entry === "object" &&
			entry !== null &&
			typeof (entry as Finding).title === "string" &&
			typeof (entry as Finding).body === "string"
		) {
			const { title, body, kind } = entry as Finding;
			findings.push(typeof kind === "string" ? { title, body, kind } : { title, body });
		}
	}

	return { findings };
}
