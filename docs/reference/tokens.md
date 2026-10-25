# Token scopes reference

What each credential must be allowed to do, and how `agent-harness doctor`
checks it. Every credential lives on the host. Only the Claude credential
enters a container (ADR-0002).

## `GH_TOKEN`

| Requirement | Why |
|---|---|
| a **classic** personal access token | A fine-grained token can push and open a PR, but no fine-grained permission can read check runs, so the post-PR CI watch is blind. |
| `repo` scope | push branches, open and read PRs, read check runs and logs, re-run failed jobs; on the GitHub tracker, read, label, assign, comment on and open issues |
| `workflow` scope, if a branch may change `.github/workflows/` | GitHub refuses a push that edits a workflow file without it |
| SSO-authorized for the org, if the org enforces SSO | otherwise the org's repos answer as if the token can't see them |

Create one at
<https://github.com/settings/tokens/new?scopes=repo,workflow&description=agent-harness>.

The same token pushes, runs `gh`, works GitHub Issues, and files
[upstream findings](../how-to/upstream-findings.md).

**What `doctor` checks:** a `github_pat_…` token fails as fine-grained without a
network call. Otherwise it calls `GET https://api.github.com/user` and reads the
`X-OAuth-Scopes` header. No header means fine-grained, no `repo` means fail, and
no `workflow` means warn. `doctor` doesn't check SSO. With `[tracker] kind = "github"` it
also reads the repo and every label the config names.

## Claude credential

Set exactly one. See [choose a Claude credential](../how-to/claude-credential.md).

| Variable | Shape | What `doctor` checks |
|---|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | `sk-ant-oat01-…`, from `claude setup-token` | the prefix only; no network call |
| `ANTHROPIC_API_KEY` | an API key from the Anthropic console | a live `GET /v1/models`; fails on 401 or 403, and fails at once on an `sk-ant-oat01-` value |

When both are set, `doctor` warns: Claude Code uses `ANTHROPIC_API_KEY`, so
sessions bill the API key.

## `LINEAR_API_KEY`

A personal API key from <https://linear.app/settings/account/security>, for an
account that can read the team's issues, update their state and description,
comment, and create issues (Findings).

**What `doctor` checks:** one GraphQL call that reads the key's own user, the
team named by `team_key`, and the labels the config names.

## `JIRA_BASE_URL`, `JIRA_EMAIL`, `JIRA_API_TOKEN`

An Atlassian API token from
<https://id.atlassian.com/manage-profile/security/api-tokens>, used with Basic
auth as `JIRA_EMAIL`. That account needs to browse the project, run JQL
searches, transition issues, edit and comment on them, and create issues of
`findings_issue_type` in `project_key`.

**What `doctor` checks:** `GET /rest/api/3/myself`, then
`GET /rest/api/3/project/<project_key>`.

## Offline

`agent-harness doctor --offline` skips every network call. A credential it can't
verify offline reports `set; not verified (--offline)`.
