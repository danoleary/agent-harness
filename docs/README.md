# Agent Harness documentation

The docs follow [Diátaxis](https://diataxis.fr/), so each page does one of four
jobs. Pick the kind of page that fits what you need right now.

## Tutorial: learn by doing

- [Your first ticket to a pull request](tutorial.md): install the harness, set
  up one project, and watch one ticket become a PR.

## How-to guides: get one task done

Setting up:

- [Adopting the harness in your project](CONSUMER.md): what a project commits,
  and what stays on your machine.
- [Use GitHub Issues as the tracker](how-to/github-issues.md)
- [Use Linear as the tracker](how-to/linear.md)
- [Use Jira as the tracker](how-to/jira.md)
- [Choose a Claude credential: API key or subscription](how-to/claude-credential.md)
- [Write the prompt bodies](how-to/prompt-bodies.md)
- [Build a custom sandbox image](how-to/sandbox-image.md)

Running:

- [Run more than one instance of the ticket loop](how-to/run-several-instances.md)
- [Fix open PRs with `pr-doctor`](how-to/fix-open-prs.md)
- [Change a loop or declare your own](how-to/declare-a-loop.md)
- [Send harness findings upstream](how-to/upstream-findings.md)
- [Troubleshoot what `doctor` can't see](troubleshooting.md)

## Reference: look something up

- [`config.toml` keys](reference/config.md)
- [Environment variables](reference/env.md)
- [Token scopes](reference/tokens.md)
- [CLI commands](reference/cli.md)
- [Error messages and exit codes](reference/errors.md)

## Explanation: understand why

- [DESIGN.md](DESIGN.md): how the harness works, its invariants and its failure
  handling.
- [Architecture decision records](adr/): one file per decision, with the context
  and the alternatives it ruled out.
- [CONTEXT.md](../CONTEXT.md): the vocabulary (Stage, Gate, Lease, Landing,
  Disposition, …) every other page uses.
