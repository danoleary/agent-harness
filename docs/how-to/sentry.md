# Give sessions the Sentry issues a ticket cites

A ticket that says "read the Sentry events for HERD-3P" can't be done inside the
sandbox: the sandbox reaches only Anthropic, and the Sentry token stays on your
machine. With a `[sentry]` table the harness reads each cited issue itself, on
the host, when it dispatches the implementation and review sessions. It puts what
it read into their prompts under "Sentry context", after the ticket. The session
is told that this is a snapshot from dispatch time.

## Turn it on

Add a `[sentry]` table to `.agent-harness/config.toml`:

```toml
[sentry]
org = "acme"            # the slug in https://acme.sentry.io
projects = ["HERD"]     # the short-ID prefixes to look for: HERD-3P
# base_url = "https://sentry.acme.dev"   # self-hosted only; default https://sentry.io
```

Each key is listed under [`[sentry]`](../reference/config.md#sentry). Without the
table nothing changes: no Sentry call is made and the prompts read as before.

## Make the token

Create an auth token at <https://sentry.io/settings/account/api/auth-tokens/>
with these scopes:

| Scope | Why |
|---|---|
| `event:read` | read the issue and its recommended event |
| `project:read` | resolve a short ID to its project's issue |
| `org:read` | the one cheap call `doctor` checks the token with |

Set it as `SENTRY_AUTH_TOKEN` in your [env file](../reference/env.md). It never
enters a container. Run `agent-harness doctor`: its `Sentry` line says whether
Sentry accepts the token for `org`.

## What a ticket can cite

- A short ID whose prefix is in `projects`, such as `HERD-3P`. Only the listed
  prefixes count, so a tracker Key such as `BEH-1148` is never looked up.
- An issue URL on your site: `https://acme.sentry.io/issues/4504/` or
  `https://sentry.io/organizations/acme/issues/4504/`.

The harness reads at most 5 issues per ticket and names any others as not
fetched. For each issue it includes the title, status, level, event and user
counts, first and last seen, culprit and link, plus the recommended event cut to
8 KiB.

## What is removed first

Event data carries user PII, so before anything reaches a prompt the harness
drops the event's user object, any `User` section and `user.*` lines in the
event, and every email and IP address.

## When an issue can't be read

A missing token, an HTTP error, or an ID Sentry doesn't know never stops the
ticket. The run log says
`⚠ <Key> cites Sentry HERD-3P but it could not be fetched: <reason>`, and the
prompt names the issue as not fetched. The session is told to report the
measurement as missing instead of guessing it.

Sentry **metrics** (a query such as `herd.secrets.load`) are not fetched. Only
issues are.
