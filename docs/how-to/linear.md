# Use Linear as the tracker

The harness reads and claims Linear issues from one team. `agent-harness init`
writes a GitHub `[tracker]`; replace it as below.

## 1. Get an API key

Create a personal API key at <https://linear.app/settings/account/security> and
put it in `.agent-harness/.env`:

```bash
LINEAR_API_KEY=lin_api_...
```

It stays on the host. You still need `GH_TOKEN` for pushes and PRs.

## 2. Create the labels

In Linear, under Settings → Labels, create:

- a ready label, such as `ready-for-agent`, that a person applies to make an
  issue eligible;
- a findings label, such as `agent-harness`, that the harness applies to the
  Findings it files.

## 3. Look up the findings label's UUID

Linear's create API takes the label's UUID, while its issue filter takes the
name, so the config needs both. Ask the API for the UUID:

```bash
curl -s https://api.linear.app/graphql \
  -H "Authorization: $LINEAR_API_KEY" -H "Content-Type: application/json" \
  -d '{"query":"{ issueLabels(filter: {name: {eq: \"agent-harness\"}}) { nodes { id name } } }"}'
```

## 4. Configure `[tracker]`

In `.agent-harness/config.toml`:

```toml
[tracker]
kind = "linear"
team_key = "PROJ"                   # REQUIRED: the team to select from
ready_label = "ready-for-agent"     # REQUIRED: the human-applied gate
findings_label_id = "<label-uuid>"  # from step 3
findings_label = "agent-harness"    # the same label, by name
# blocked_label = "Blocked"         # optional: screens an issue out until a person removes it
```

`team_key` and `ready_label` are checked at startup, because neither has a safe
default: an empty team key would query every team your key can see, and an
empty ready label would make every unstarted issue eligible.

## 5. Check it

```bash
agent-harness doctor
agent-harness pipeline --next --dry-run
```

`doctor` checks the key, that the team is visible to it, and that the labels
exist.

## Hold a ticket until another one lands

Add a `blocks` relation from the blocker and leave the ticket labelled ready.
The harness never picks an issue a still-open issue blocks, so it's picked up on
the first poll after its last blocker closes. To hold a ticket for a human
reason, leave the ready label off.

The harness also holds a ticket, on every tracker, when its title or body names
a file that an open harness PR or a claimed ticket's worktree already changes.
The loop log shows `#97 deferred: overlaps PR #81 on <files>`, and the ticket is
picked up on the first poll after that work merges or closes, so sibling tickets
that edit one module run one after another. Label a ticket `parallel-ok`
(`OVERLAP_ESCAPE_LABEL`) to run it anyway, or set `OVERLAP_THRESHOLD=0` to turn
the check off (ADR-0032).

## See also

- [`[tracker]` keys](../reference/config.md#tracker)
- [Token scopes](../reference/tokens.md)
