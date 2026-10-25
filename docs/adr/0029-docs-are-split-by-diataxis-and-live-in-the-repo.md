# ADR-0029: The docs are split by Diátaxis and live in the repo, not a wiki

- Status: Accepted
- Date: 2026-10-25

## Context

By #57 a Consumer's docs were three files. `docs/CONSUMER.md` was 540 lines of
setup steps, a commented `config.toml`, the secrets table, the loop reference and
the pr-doctor guide, all in one page. `docs/troubleshooting.md` held the fixes
`doctor` can't make. `docs/DESIGN.md` explained the design. A newcomer had no
end-to-end walkthrough. Someone who needed one fact, such as a key's default or
an exit code, had to read prose to find it. Nothing listed the env vars the code
reads, and `.env.example` had already drifted from the defaults in
`internal/config`.

## Decision

**`docs/` follows [Diátaxis](https://diataxis.fr/). Each page is one of four
kinds, and `docs/README.md` indexes them.**

| Kind | Pages |
|---|---|
| Tutorial | `docs/tutorial.md`: the first ticket to a PR, end to end |
| How-to | `docs/how-to/*.md`, one task each (a tracker's setup, the Claude credential, prompt bodies, the sandbox image, instances, pr-doctor, loops, upstreaming), plus `docs/troubleshooting.md` |
| Reference | `docs/reference/*.md`: `config.toml` keys, env vars, token scopes, CLI commands, error messages |
| Explanation | `docs/DESIGN.md`, `docs/adr/`, `CONTEXT.md`, unchanged |

`docs/CONSUMER.md` keeps its path, because the install, the release archive,
`doctor`, `--help` and the config loader's errors all point at it. It is now the
short adoption page: what a Consumer commits, what stays on the host, and links
to the rest. The sandbox base-image pin moves with the Dockerfile example to
`docs/how-to/sandbox-image.md`, and `scripts/release.sh` bumps it there.

`internal/docs` tests the tree. Every relative link must resolve, every `#anchor`
must name a heading, and `docs/README.md` must link every page. A page that
moves or is renamed fails `make check` instead of leaving a dead link.

## Alternatives considered

- **A GitHub wiki.** GitHub lets search engines index a wiki only when the repo
  has 500+ stars and public editing is disabled, and otherwise serves it with
  `X-Robots-Tag: none`. A wiki is also a separate repo, so the docs can't change
  in the same PR as the behaviour they describe, and they drift from releases.
  Markdown in `docs/` renders on GitHub with no build step.
- **A generated site (GitHub Pages).** It can come later and read the same
  files. Nothing needs it yet.
- **Moving `CONSUMER.md`'s content and deleting it.** That would break every
  link already printed by an installed binary, and the file the release archive
  ships.

## Consequences

- The release archive and the install still ship only `CONSUMER.md`. Its links
  into `how-to/` and `reference/` work on GitHub, not offline from
  `~/.local/share/agent-harness/`.
- A change to a config key, env var, CLI flag or operator-facing error updates
  the matching reference page in the same PR. The link test only catches a page
  that moved or a broken link. It doesn't catch a reference table that has drifted
  from the code.
