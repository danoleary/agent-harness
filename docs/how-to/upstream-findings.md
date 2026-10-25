# Send harness findings upstream

The retrospective stage classifies each Finding by **audience**. A *Project*
finding is about your codebase or CI and goes to your tracker. A *Harness*
finding is about the harness, sandbox or contract itself, and by default stays
local in `.agent-harness/harness-findings/`.

## Review the local findings

Read `.agent-harness/harness-findings/` from time to time. A new finding whose
key or title closely matches one already there is still written, but carries
`probable_duplicate_of: <slug>`. Merge or delete it.

## Opt in to filing them upstream

Read ADR-0011 first if your codebase is proprietary: the destination is a
**public** repo. Findings are sanitized before filing, but you are the one
publishing them.

Add a `[feedback]` table to `.agent-harness/config.toml`:

```toml
[feedback]
upstream = "github"
repo = "danoleary/agent-harness"
findings_label = "harness-finding"
project = "myproject"
```

There is no separate credential: the harness files the issues with your
`GH_TOKEN`, so they appear under that token's account (see
[token scopes](../reference/tokens.md)). Each key is listed under
[`[feedback]`](../reference/config.md#feedback).
