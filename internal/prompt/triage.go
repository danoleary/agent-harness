package prompt

// TriageVerdictFile is where a Triage session writes its verdict, under the
// /findings dropbox the host mounts for it (ADR-0045). The host reads it back.
const TriageVerdictFile = "verdict.json"

// triageBar is the envelope's default bar for "ready for an agent". It leans
// toward ready: a ticket is held back only for something an agent cannot do or
// decide alone, so triage does not become a second backlog of nitpicks. A
// Consumer body can tighten it with its own conventions.
const triageBar = `A ticket is READY when an agent could implement it unattended, in one branch and one PR, from the ticket text plus this repository alone. Lean toward ready: a ticket that is terse but whose intent and done-state are clear from the code is ready. It is NOT ready only when at least one of these holds:
  1. The goal or the done-state is unclear: two reasonable agents would build materially different things, and the code does not settle which.
  2. It needs a person's decision between designs, product behaviours or trade-offs that the ticket leaves open (an "Open questions" section still unanswered counts).
  3. It needs something an agent in a sandbox cannot do: a credential or secret, a step in a dashboard or other UI, access to production data, or a purchase or account change.
  4. It is too large for one PR and has no sub-issues splitting it, or it is an umbrella, a discussion, a question, or a bug report with no reproduction an agent could follow.
  5. It depends on work not yet merged, or the code shows it is already done (then say so: a human should close it).`

// triageVerdictProtocol is the verdict contract the host checks.
const triageVerdictProtocol = "Write your verdict to `/findings/" + TriageVerdictFile + "` as ONE JSON object: `{\"ready\": true|false, \"reason\": \"...\", \"questions\": [\"...\"]}`. `reason` is one or two sentences a human reads on the ticket, saying why. When `ready` is false, `questions` lists each thing a human must answer or do before an agent can start, one per entry, phrased so the human can answer it directly; it must not be empty. When `ready` is true, `questions` is `[]`. **Always write the file**: an absent or malformed file means triage never ran, and the ticket is judged again on the next poll."

// triageContract is the read-only, tracker-off half of the envelope.
func triageContract(kind string) string {
	return join(
		"This session is read-only and reaches no remote. Read the repository to check the ticket against the code (that the files and behaviour it names exist, and that it is not already done), but make NO changes: do not edit, create or delete files in the checkout, do not create a branch or worktree, and do not commit or push. The only file you write is the verdict.",
		trackerOff(kind)+" The harness labels the ticket or comments your questions on it from your verdict.",
	)
}

// triage builds the `-p` prompt for the sandboxed Triage session over one backlog
// ticket (ADR-0045). The Consumer body, when the loop's assess step supplies one,
// leads with the project's own bar; the envelope always carries the default bar,
// the ticket, the verdict contract and the read-only steer.
func triage(c Context) string {
	return join(
		body(c),
		"Decide whether the ticket below is ready for an autonomous coding agent to start work on. You are triaging, not implementing.",
		triageBar,
		ticketContext(c.Ticket),
		"---",
		triageVerdictProtocol,
		triageContract(c.Tracker),
		bashQuirkSteer,
	)
}
