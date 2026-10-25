# Adopting the harness in your project

A project that uses the harness is a **Consumer**. You adopt it by committing an
`.agent-harness/` directory, never by forking the harness (ADR-0008). New to
it? Start with the [tutorial](tutorial.md).

The harness runs on your machine, outside your repo. It bind-mounts your checkout
into a container for each Stage and does every remote operation (push, PR,
tracker) from the host.

## What you commit

```
.agent-harness/
  config.toml      non-secret settings: image, gates, post_create, tracker, loops
  Dockerfile       your sandbox image, FROM a pinned harness base
  prompts/
    implement.md   one Project body per Stage, each naming a skill your repo carries
    review.md
    retro.md
```

`agent-harness init` writes all of it, with every guessed value marked
`TODO(agent-harness init)`.

- Every key in `config.toml`: [config reference](reference/config.md).
- The prompt bodies: [write the prompt bodies](how-to/prompt-bodies.md).
- The Dockerfile: [build a custom sandbox image](how-to/sandbox-image.md).

## What stays on your machine

The harness itself, and the credentials, which are host-only environment
variables, usually in a gitignored `.agent-harness/.env`. Only the Claude
credential ever enters a container; the tracker and GitHub credentials stay on
the host, so a compromised session can't move your backlog or push to your
remote (ADR-0002).

- Every variable, and where the harness looks for the file:
  [environment variables](reference/env.md).
- What each token needs: [token scopes](reference/tokens.md).
- API key or subscription: [choose a Claude credential](how-to/claude-credential.md).

## Choosing a tracker

The tickets come from one of three trackers (ADR-0010):
[GitHub Issues](how-to/github-issues.md), [Linear](how-to/linear.md) or
[Jira](how-to/jira.md). In each, a person applies a ready label (or, in Jira,
matches a JQL query) to decide what the harness works unattended.

## Everything else

The [documentation index](README.md) lists every guide and reference page.
