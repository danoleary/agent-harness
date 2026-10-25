# Use Jira as the tracker

The harness works Jira Cloud tickets through the REST API with Basic auth. The
queue is a JQL query you write, and claiming and releasing are workflow
transitions you name. `agent-harness init` writes a GitHub `[tracker]`; replace
it as below.

## 1. Get an API token

Create an API token at <https://id.atlassian.com/manage-profile/security/api-tokens>
for the account the harness should act as. Put the site, the account's email and
the token in `.agent-harness/.env`:

```bash
JIRA_BASE_URL=https://acme.atlassian.net
JIRA_EMAIL=bot@acme.co
JIRA_API_TOKEN=...
```

They stay on the host. You still need `GH_TOKEN` for pushes and PRs.

## 2. Find your transition names

Open a ticket in the project and look at the status dropdown. Each option you
can move it to is a transition, by name. You need three:

- the one that moves a ready ticket to In Progress (the claim);
- the one that moves an In Progress ticket back to To Do (the release);
- the one that closes a ticket as won't-do (used when the review recommends
  closing a ticket whose work is already done).

Names, not ids: the harness resolves the name against the ticket's available
transitions at the moment it uses one.

## 3. Configure `[tracker]`

In `.agent-harness/config.toml`:

```toml
[tracker]
kind = "jira"
project_key = "PROJ"
ready_jql = 'project = PROJ AND status = "To Do" AND labels = ready-for-agent AND assignee IS EMPTY ORDER BY rank'
in_progress_jql = 'project = PROJ AND status = "In Progress" AND labels = ready-for-agent'
in_progress_transition = "In Progress"
todo_transition = "To Do"
cancel_transition = "Won't Do"
findings_label_id = "agent-harness"   # a label NAME, applied to filed Findings
# findings_issue_type = "Task"        # the issue type Findings are created as
```

`ready_jql` is the whole selection rule: the harness takes its first result. Put
the ready label, any "blocked" screen and the order in the query. Jira issue
links don't hold a ticket on their own; exclude blocked tickets in the JQL.

`in_progress_jql` is what the stale-claim reaper sweeps. Keep it narrow enough
to match only tickets the harness claimed.

## 4. Check it

```bash
agent-harness doctor
agent-harness pipeline --next --dry-run
```

`doctor` signs in and checks it can see `project_key`. Loading the config fails
if `project_key`, either JQL key or any of the three transitions is missing. A
misspelled transition name is only caught when the harness uses it: it fails the
claim with
`transition "In Progres" not available on issue PROJ-1`.

## See also

- [`[tracker]` keys](../reference/config.md#tracker)
- [Token scopes](../reference/tokens.md)
