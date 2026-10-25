# ADR-0028: A ready ticket with open questions is dispatched with a warning

- Status: Accepted
- Date: 2026-10-25
- Renumbered: from ADR-0024 by #92 — #89 took 0024 in parallel; then from ADR-0026 by #96 — #29 took 0026 in parallel
- Extends: ADR-0009

## Context

#55 was labelled ready-for-agent and dispatched. Its body ended with a section
titled "Open questions (for a human before this is agent-ready)". That section
said the per-toolchain defaults "need to be decided, not invented by the agent".
Nothing in the run log or the prompt pointed at it, so the implementation session
had to choose by itself between stopping and deciding the questions. It decided
them, recorded the answers in ADR-0020, and flagged them for review.

## Decision

**A ready-labelled ticket whose body still has a section of questions for a
human is dispatched. The run log warns about it, and the envelope tells the
session how to handle the questions.**

- **Detection (`ticket.Ticket.OpenQuestions`).** A section counts if its heading
  is a Markdown heading or a line that is all bold, and the heading names open or
  unresolved questions, questions "for a human", or a ticket not yet agent-ready.
  These do not count: a heading in a fenced block, a heading marked resolved,
  answered or decided, and a section whose content is empty or "None". The same
  phrase in running prose is not a section, so it does not count either.
- **Advisory.** The implementation stage logs the headings next to the
  resolved-symbol advisory. It never skips the ticket.
- **Envelope.** Every Implement variant names the headings. It tells the session
  not to answer the questions silently. If the work does not need a question
  settled, the session leaves it open and says so in the handoff. If the work does
  need it, the session makes the smallest decision it can defend, records it (in
  an ADR when it is a design choice), and lists it in the handoff and commit body.

## Alternatives considered

- **Skip the ticket at selection, as ADR-0023 does for an open prerequisite.** The
  ready label is a human's decision, and the questions are often minor or already
  answered in a comment. A false skip drops real work without any visible signal.
  A wasted or over-decided session is the cheaper error, and review catches it.
- **Tell the session to stop and recommend close.** The questions do not mean the
  work is invalid, so stopping only delays it. #55 showed that a session can
  decide reasonably as long as its decisions are flagged.

## Consequences

- The heuristic matches headings only. An open question written into a paragraph
  produces no warning.
- A ticket that names a command or file that does not exist yet (#55 also named
  `agent-harness doctor`) is not detected by this check.
