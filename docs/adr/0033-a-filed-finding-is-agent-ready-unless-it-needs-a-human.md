# ADR-0033: A filed finding is agent-ready unless it needs a human

- Status: Accepted
- Date: 2026-10-27
- Amends: ADR-0010, ADR-0011

## Context

The GitHub adapter filed every project-audience finding with the findings label
only. The ready label was described as a human-applied gate, so nothing the
retrospective filed reached the queue until someone relabelled it. On 27 Oct
2026 the loop had drained every labelled ticket. Six open findings sat idle, and
most of them were fixes an agent could make alone.

## Decision

**The harness files a finding with the ready label unless the finding needs a
human or reads as a probable duplicate of an open one.**

- **Classification at source.** The retrospective writes `needs_human: true`
  only when the fix needs a person: a choice between designs, a credential or
  secret, or a step in a dashboard or other UI. Absent means `false`.
- **Router.** `filing.Router` sets `FileFindingOptions.Ready` to
  `!needs_human`, and to `false` when the finding is flagged as a probable
  duplicate (#65), since a person should merge or close it first.
- **Adapter.** The GitHub adapter adds its configured ready label when `Ready`
  is set. The upstream sink has no ready label configured, so findings filed into
  the public harness repo from other projects stay behind a human.

## Alternatives considered

- **Keep the ready label human-only.** That is the safer blast radius, but it
  leaves the queue empty while agent-sized work waits on a relabel that adds no
  judgement for most findings.
- **Ready by default with no opt-out.** Some findings ask for a design decision,
  which the session would then make by itself (the ADR-0028 problem).

## Consequences

- Linear and Jira findings are still filed without ready. Linear's ready marker
  is a label name that `FileFinding` would have to resolve to an id, and Jira's
  is a JQL query with no single field to set. Each can follow the same `Ready`
  option when it is needed.
- A finding the session misclassifies runs unattended. Review and the PR gates
  still apply, and ADR-0028 warns on any open-questions section left in its body.
