# Use GitHub Issues as the tracker

GitHub Issues is the tracker `agent-harness init` writes by default. The tickets
are the issues of one repository that a person has labelled ready.

## 1. Get a token

Create a **classic** personal access token with the `repo` scope, and authorize it
for SSO if your organization uses SSO. Put it in `.agent-harness/.env`:

```bash
GH_TOKEN=ghp_...
```

The same token pushes branches, opens PRs, watches CI and works the issues. A
fine-grained token can't read check runs, so `doctor` fails it; see
[token scopes](../reference/tokens.md).

## 2. Configure `[tracker]`

In `.agent-harness/config.toml`:

```toml
[tracker]
kind = "github"
repo = "myorg/myproject"            # owner/name; init takes it from your origin remote
ready_label = "ready-for-agent"     # a person applies this to make an issue eligible
in_progress_label = "in-progress"   # the harness adds it on claim and removes it on release
findings_label_id = "agent-harness" # a label NAME on GitHub, applied to filed Findings
# blocked_label = "blocked"         # optional: screens an issue out until a person removes it
# assignee = "my-bot-account"       # optional: also assign the issue on claim
```

## 3. Create the labels

The harness doesn't create labels. Create each one you named:

```bash
gh label create ready-for-agent --repo myorg/myproject
gh label create in-progress     --repo myorg/myproject
gh label create agent-harness   --repo myorg/myproject
```

`agent-harness doctor` checks that every label the config names exists.

## 4. Mark a ticket ready

An issue is eligible when it is open, unassigned and carries `ready_label`, and
it doesn't carry `blocked_label`, isn't blocked by an open issue, and doesn't
name an open PR it builds on. The harness takes the oldest eligible issue first,
and won't claim one that already carries `in_progress_label`.

```bash
gh issue edit 42 --add-label ready-for-agent --repo myorg/myproject
agent-harness pipeline --next --dry-run    # shows the issue it would pick
```

## Hold a ticket until another one lands

- **On another issue:** link it natively ("blocked by" in the issue's
  relationships) and leave it labelled ready. It's picked up on the first poll
  after its last blocker closes.
- **On an open PR:** name the PR in the body, for example `builds on #81`. The
  ticket waits until that PR merges or closes (ADR-0023).
- **For a human reason:** leave the ready label off.

## See also

- [`[tracker]` keys](../reference/config.md#tracker)
- [Fix open PRs with `pr-doctor`](fix-open-prs.md), which uses your
  `in_progress_label` and `blocked_label` names on GitHub
