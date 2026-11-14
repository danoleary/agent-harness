# ADR-0058: Host git refuses a command the repository's config names

- Status: Accepted
- Date: 2026-11-14
- Amends: ADR-0057 (closes the filters and command-valued keys it left open)

## Context

ADR-0057 gave every host-side git `-c core.hooksPath=/dev/null -c
core.fsmonitor=false`. A `-c` override needs the key's name. Some command-valued
keys have a name the session picks (#351). A session writes `[filter "x"] smudge
= <cmd>` to `.git/config` and `* filter=x` to `.gitattributes`. The host's next
`worktree add`, `reset --hard` or `cherry-pick` then runs `<cmd>`, next to
`GH_TOKEN`. Merge drivers (`merge.<name>.driver`) and diff drivers
(`diff.<name>.textconv`, `diff.<name>.command`) work the same way, and the host's
`cherry-pick`, `diff --binary` and `log -S` run them. Other keys have fixed names
but no value that means "off": `diff.external`, `core.sshCommand`,
`credential.helper`, `remote.<name>.uploadpack`, `gpg.program`.

## Decision

**Before every host-side git command, the host lists the config git would read
there. It refuses to run the command while a scope a session can write sets a
command-valued key.**

- The listing is `git [-C dir]... config --list --includes --show-scope
  --show-origin -z`, using the command's own `-C` options. The `local` and
  `worktree` scopes are the ones a session can write, files they include among
  them. `global` and `system` belong to the operator, so a filter set there,
  Git LFS's included, still runs.
- The keys are `filter.<name>.{clean,smudge,process}`, `diff.external`,
  `diff.<name>.{command,textconv}`, `merge.<name>.driver`,
  `core.{sshCommand,pager,editor,askPass,gitProxy,alternateRefsCommand}`,
  `pager.*`, `sequence.editor`, `credential[.<url>].helper`,
  `remote.<name>.{uploadpack,receivepack}`, `gpg[.<format>].program` and
  `gpg.ssh.defaultKeyCommand`. An empty value names no command and is allowed:
  `credential.helper =` is how a config resets the helper list.
- The refusal is an error from the git call, naming the file and the key. It
  fails whatever Stage asked for the call. The operator removes the key, or
  moves it to their global config.
- The check lives in `internal/git`'s production runners, beside ADR-0057's
  overrides. `git.HostArgs` now runs it too and returns an error, so every host
  git call outside a `Checkout` gets it. Those calls name their directory with
  `-C`, because the check reads that directory's config.
- Three more keys have fixed names, so the host overrides them with `-c`:
  `protocol.ext.allow=never` (an `ext::` remote URL runs as a command),
  `submodule.recurse=false` and `fetch.recurseSubmodules=false`. A submodule's
  git reads the submodule's own config under `.git/modules`, and the check does
  not list it.
- If the listing itself fails, the command runs as asked. Anything that stops
  git listing its config, such as a missing directory or a config it cannot
  parse, also stops the command from reading it. The caller then gets git's own
  error.

## Alternatives considered

- **Override each key with `-c`.** A filter or driver name is the session's
  choice, so no fixed list can name it. Several keys also have no value that
  turns them off.
- **A host git that reads no repository config.** Git has `GIT_CONFIG_NOSYSTEM`
  and `GIT_CONFIG_GLOBAL`, but no switch for the repository's own file. Host git
  also needs that file for `remote.origin.url`, the worktree list and the
  repository format.
- **Neutralise rather than refuse.** An emptied filter checks out an LFS pointer
  in place of the file, and a quiet change like that is worse than an error that
  names the key.

## Consequences

- A Consumer that sets a filter, credential helper or `core.sshCommand` in
  `.git/config` must move it to global config before the harness will run there.
  `git lfs install` writes to global config by default; `--local` does not.
- Every host git command now costs one extra `git config --list`.
- The check runs just before the command, not atomically with it. A session that
  is running while the host runs git could write a key between the two. Host git
  on a ticket's worktree runs before or after its session. The window that is
  left is a sibling session writing the shared `.git/config` during another
  ticket's host git call.
- Still not closed: the `git` that a host-run Consumer command starts
  (`prune_command`, the prune script, `gh`) gets ADR-0057's overrides through
  `git.HostEnv` (#350), but not this check.
