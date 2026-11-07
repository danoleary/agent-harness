# ADR-0041: The host fetches the Sentry issues a ticket cites into the prompt

- Status: Accepted
- Date: 2026-11-07
- Extends: ADR-0009, ADR-0013
- Keeps: ADR-0002

## Context

A herd ticket whose first step was "read the Sentry events for HERD-3P" could not
be done in the sandbox (BEH-1157, seen in BEH-1148 and BEH-1179). The `sentry`
MCP server needs interactive OAuth, and under ADR-0002 the sandbox reaches only
Anthropic anyway. The session shipped a fix without the measurement and recorded
its cause as unknown.

The tracker body had the same problem and already has an answer:
`ticketContext` puts the body into the envelope because the session can't fetch
it.

## Decision

**When the Consumer config has a `[sentry]` table, the harness reads the Sentry
issues the ticket body cites, on the host, and the implementation and review
envelopes carry them in a "Sentry context" section after the ticket context**
(#211).

- **Config.** `[sentry]` names the `org`, the short-ID `projects` prefixes and an
  optional `base_url` (default `https://sentry.io`). The token is the host env's
  `SENTRY_AUTH_TOKEN`. Without the table no Sentry call is made and every prompt
  is byte-identical to before.
- **Detection (`sentry.Detect`).** The harness collects short IDs whose prefix is
  in `projects`, and issue URLs on the site or the org's `<org>.sentry.io`
  subdomain. The prefix list is what keeps a tracker Key such as `BEH-1148` from
  being looked up. At most 5 issues per ticket are fetched, and the rest are named
  as not fetched.
- **Fetch.** The fetch sits behind the host port (`hostio.Remote.SentryIssue`,
  ADR-0013), so the stages test against `hostio.Fake`. It reads the group
  (`shortids/{id}` or `issues/{id}`) and the recommended event in Sentry's LLM
  markdown, with a 10-second timeout per request.
- **PII.** The event's user object is never decoded. `sentry.Gather` is the only
  way an issue reaches the prompt. It removes any `User` section and `user.*`
  lines and redacts every email and IP address in what it keeps. It also redacts
  credentials: the values of `Authorization`/`Cookie`/`Set-Cookie`/`X-Api-Key`
  headers, bearer tokens, `X-Amz-*`/`X-Goog-*` signing parameters and secret-named
  keys (`*token`, `*api_key`, `*secret`, `*password`, `*sig`, `*signature`),
  because Sentry's server-side scrubbing is per-org config the harness can't
  rely on. Then it cuts the event to 8 KiB. The section is defanged like the
  ticket body, and it tells the session that the data is a snapshot taken at
  dispatch time.
- **Untrusted event text.** Anyone who can make the application fail can write
  into an event, so the event is less trusted than the ticket body. It is fenced
  with a tilde fence longer than any tilde run in it, so no line of the event can
  close the fence and pass as envelope prose, and it is labelled as data, not
  instructions. This does not stop a model from following text in it; the
  sandbox's network (ADR-0002) bounds what a steered session can send, and the
  review stage and a human merge bound what it can ship.
- **Failures never block dispatch.** A missing token, an HTTP error or an unknown
  ID logs `⚠ <Key> cites Sentry <id> but it could not be fetched: <reason>`. The
  envelope then names each unfetched issue, so the session reports the
  measurement as missing instead of guessing it.
- **Doctor.** With `[sentry]` set, `doctor` checks that the token is set and that
  `GET /api/0/organizations/<org>/` accepts it.

ADR-0002 stands: `sandbox.SecretEnv` is unchanged and the Sentry token never
enters a container.

## Alternatives considered

- **Forward a Sentry token into the container and let the session query
  Sentry.** That would need an ADR-0002 amendment to open the sandbox's network
  to Sentry. It would also put event data, which carries user PII, unfiltered
  inside a `--dangerously-skip-permissions` session. Rejected by a human before
  this was built.
- **Fetch Sentry metrics too (`herd.secrets.load`).** The metrics and Discover
  APIs need a query, not just an ID, so the ticket body would need a query
  contract. That is a separate design and is left for a follow-up.

## Consequences

- A session that is told to read a cited issue now has the issue's counts,
  first and last seen, and recommended event in its prompt.
- The data is a snapshot. Each stage fetches its own at its own dispatch, so the
  review's copy may be newer than the implementation's.
- The scrub works on patterns: it redacts emails and IPv4 and IPv6 addresses
  wherever they appear, and it can also redact a value that only looks like one,
  such as a four-part version number. Credential redaction keys on names, so a
  secret under an unusual key name gets through, and a harmless value under a
  secret-sounding key is redacted.
- Only the ticket body is scanned, not its title or sub-issues.
