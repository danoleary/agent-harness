# Choose a Claude credential: API key or subscription

Every Stage is a Claude Code session, and the Claude credential is the only
secret that crosses into the sandbox (ADR-0002). Set **exactly one** of these in
your `.env`:

| Variable | What it is | Billed as |
|---|---|---|
| `CLAUDE_CODE_OAUTH_TOKEN` | a subscription OAuth token, `sk-ant-oat01-…` | your Claude subscription's usage |
| `ANTHROPIC_API_KEY` | an API key from console.anthropic.com, `sk-ant-api03-…` | pay-as-you-go API usage |

When both are set, Claude Code uses `ANTHROPIC_API_KEY`.

## Use a subscription

1. On any machine with the Claude CLI, run `claude setup-token` and sign in.
2. Put the token it prints in `.agent-harness/.env`:

   ```bash
   CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...
   ```

3. Run `agent-harness doctor`. The credential line makes a live call with it.

An `sk-ant-oat01-…` token **must** go in `CLAUDE_CODE_OAUTH_TOKEN`. As
`ANTHROPIC_API_KEY` it is sent as an API key and rejected as `Invalid API key`.

## Use an API key

1. Create a key at console.anthropic.com.
2. Put it in `.agent-harness/.env`:

   ```bash
   ANTHROPIC_API_KEY=sk-ant-api03-...
   ```

3. Run `agent-harness doctor`.

## How to choose

- **A subscription** costs nothing extra up to its usage limit. When the account
  hits its spending cap, a session aborts before it does any work and the run
  ends **CapAborted**. The ticket goes back to the queue, the circuit breaker
  does not count it, and the loop backs off until the reset time the abort names
  (`LOOP_CAP_BACKOFF_MS`, default 45 minutes, when it cannot read one) before it
  polls again.
- **An API key** has no cap beyond the limits you set on the console, so an
  unattended loop keeps going. It is also the only credential the host-side
  finding dedup can use: with `ANTHROPIC_API_KEY` on the host, the harness asks
  `DEDUP_MODEL` whether a new Finding repeats a filed one; with only an OAuth
  token it falls back to exact-match dedup.

## Pick the models

`HARNESS_MODEL` sets the model for every Stage. `TDD_MODEL`, `REVIEW_MODEL` and
`RETROSPECTIVE_MODEL` override it for one Stage. See
[environment variables](../reference/env.md#sessions-and-models).
