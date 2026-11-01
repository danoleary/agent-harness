# ADR-0038: A finding with no ticket files into the tracker's findings team

- Status: Accepted
- Date: 2026-11-01
- Amends: ADR-0010, ADR-0019

## Context

A project finding files into the team its ticket names (`Ticket.TeamID`). A PR
loop run (ADR-0019) works a PR, not a ticket, so nothing named a team for it. The
PR loop filled the gap only for GitHub Issues, where the team is the configured
repo. With Linear or Jira every project finding a PR-loop retrospective wrote was
dropped: in herd, `findings ✗ 1 dropped but no team id resolved for PR-1312`
(#174). The dropped findings were about host gates treating crashes as red.

## Decision

**The findings sink names the container a finding files into when no ticket
does: `FindingsSink.FindingsTeam()`. The PR loop's retrospective files its
project findings there.**

- **GitHub Issues**: the configured `repo`.
- **Jira**: the configured `project_key`, the project every ticket the adapter
  fetches already names.
- **Linear**: the configured `team_key`, resolved to the team's UUID with one
  GraphQL lookup, because the create API takes a UUID. A key no team carries is
  an error.

The PR loop asks only when a retrospective runs. A tracker that cannot be
resolved, or a team that cannot be found, is one warning line, and the run files
harness findings only, as before (ADR-0001).

## Alternatives considered

- **Resolve the ticket a harness branch names.** A PR on
  `<branch_prefix>/<slug>` names a ticket whose team could be fetched. A human's
  branch opted in by label names no ticket, so this covers only some PRs, and
  inverting a slug back to a Key is lossy. The ticket's team is the configured
  team in practice, since selection is scoped to it.
- **A new config key for a default findings team.** Every tracker already
  configures the container its queue lives in. A second key would repeat it and
  could disagree with it.

## Consequences

- The port gains one method, and every adapter implements it. GitHub and Jira
  answer from config. Linear makes one API call per PR-loop retrospective.
- The ticket loop is unchanged: a ticket still files into its own team.
