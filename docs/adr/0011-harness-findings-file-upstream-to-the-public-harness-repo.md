# ADR-0011: Harness findings are classified at source and filed upstream as issues on the public harness repo

- Status: Accepted
- Date: 2026-07-01

## Context

We want the harness to keep improving from how it behaves on *other* projects, not
only herd (ADR-0007). Today findings are *only* harness/environment findings and are
filed to herd's own Linear — which works only because herd *is* the harness's home.
Once the harness runs on many Consumers with different trackers (ADR-0010), that
conflation breaks: a Jira Consumer will not file harness-improvement issues into
your Linear, and you will not watch their Jira. Findings now have two audiences —
*about the Consumer's project* vs *about the harness itself* — and harness findings
routinely quote Consumer transcripts and code, which may be proprietary.

## Decision

**The retrospective classifies every finding by `audience: project | harness`.
Project findings route to the Consumer's Tracker (ADR-0010). Harness findings
default to a local artifact and, only on explicit opt-in, are filed as issues on the
public, maintainer-owned harness repo (the Upstream sink).**

- **Default `feedback.upstream: off`** — Harness findings are written to
  `.agent-harness/harness-findings/` in the Consumer repo for the owner to review.
  Nothing leaves the repo without consent.
- **Opt-in `feedback.upstream: github`** — the harness files Harness findings as
  issues on the configured public harness repo, **using the Consumer's existing
  `GH_TOKEN`** (a public repo only needs `public_repo` scope; this also attributes
  the issue to the reporting project, which is useful provenance). Dedup by `key`
  across all projects; a re-occurrence from another project becomes a comment/
  reaction tagged with the project name, not a fresh issue.
- **Sanitize rule in the envelope** — because the sink is *public*, the retrospective
  contract envelope (ADR-0009) carries a non-overridable rule: a Harness finding
  states the failure class and harness-side detail only — never Consumer source,
  transcripts, or secrets. This cannot be weakened by a Consumer's prompt body.

## Alternatives considered

- **Telemetry endpoint / service.** Most reach, but most infra and the hardest
  privacy story. Rejected for an open-source GitHub repo the maintainer already has.
- **Dedicated feedback token.** Decouples provenance from the Consumer's identity,
  but is another secret to provision and makes every report look like one bot.
  Rejected — reuse `GH_TOKEN`.
- **Local-only, manual upstreaming.** Zero leak, but collection is all-manual and
  you get little back. Kept as the *default*, not the only mode.

## Consequences

- Cross-project feedback arrives only from Consumers that opt in — by design; the
  alternative is silently exfiltrating private code to a public repo.
- The retrospective must reliably classify audience; a misclassified Project finding
  that upstreams could leak Consumer detail, so the sanitize rule is the backstop
  regardless of classification.
- herd dogfoods through the same path: its Harness findings upstream like any other
  Consumer's, so the loop is exercised continuously.
