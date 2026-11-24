# Build a custom sandbox image

Every Stage, every Gate and your `post_create` hook run in your project's
sandbox image. That image layers your toolchain on the harness's published
**sandbox base image**, which carries the Claude CLI, `git`, `gosu` and the
uid-re-exec entrypoint (ADR-0008). `agent-harness init` writes a starting
Dockerfile for Go, pnpm, .NET or Python; this guide is for changing it or writing
your own.

## 1. Write a Dockerfile `FROM` a pinned base

Save it as `.agent-harness/Dockerfile`:

```dockerfile
FROM ghcr.io/danoleary/agent-harness-base:0.13.0

# Your toolchain. Everything here is yours; the base carries only the
# harness<->sandbox contract (claude CLI, git, gosu, the uid-re-exec entrypoint).
RUN apt-get update \
	&& apt-get install -y --no-install-recommends dotnet-sdk-9.0 \
	&& rm -rf /var/lib/apt/lists/*

# A tool installed outside /usr/bin goes on PATH here, never in a profile script:
# ENV PATH=/opt/mytool/bin:$PATH

# Image-internal paths your toolchain needs WRITABLE at run time, colon-separated.
# The container enters as root and drops to the bind-mounted checkout's owner uid,
# so anything you bake owned by the image's uid 1000 is unwritable when the host
# uid differs. The entrypoint chowns each path in this list when that happens.
#
# Your CACHE path does not go here — the harness passes that itself.
ENV HARNESS_CHOWN_PATHS=/opt/my-baked-tool
```

Pin the immutable semver tag, never `:latest`. A base bump should be a
deliberate, reviewable edit, not a silent floating pull. Each harness release
names the base it was built with.

## 2. Point `config.toml` at it

Declare **exactly one** of `dockerfile` or `image`:

```toml
image = "myproject-agent-harness:latest"   # the tag the harness builds it as
dockerfile = ".agent-harness/Dockerfile"   # build it on a local image miss, and when it changes
```

To use an image you build and publish yourself instead, set only `image` to its
ref; the harness pulls it on a miss. It still has to be built `FROM` the base.

## 3. Put every tool on the image `PATH`

The harness runs `post_create` and every Gate as a non-login `bash -c <command>`,
so `/etc/profile` and `~/.profile` are **not** sourced. A `PATH` you set only in
`/etc/profile.d/*.sh` never reaches a gate: the gate exits 127, and the run stops
as an environment failure. Use `ENV PATH=...` in the Dockerfile.

Check it before the first run:

```bash
docker build -f .agent-harness/Dockerfile -t myproject-agent-harness:latest .
docker run --rm -e PROJECT_PATH=/tmp myproject-agent-harness:latest bash -c 'command -v dotnet'
```

From a harness checkout, `scripts/check-sandbox-image.sh <image> <tool>...` checks
several tools at once.

## 4. Add a cache, if your toolchain has one

A persistent volume saves re-downloading dependencies in every container:

```toml
[cache]
volume = "myproject-nuget"
path = "/root/.nuget/packages"
prune_command = "dotnet nuget locals all --clear"   # runs on the HOST when disk is low
```

## Changing the image in a ticket

A ticket that edits this Dockerfile has its `post_create` and gates run in an
image built from the branch's copy (`<image>:branch-<fingerprint>`), so a fix to
the toolchain is gated in the image it fixes (ADR-0022). Only the Dockerfile's
own bytes are compared: a branch that changes only a file the Dockerfile `COPY`s
still runs in the current image.

## Searching the bundled Claude CLI

A session that needs to know how the pinned Claude CLI behaves (a flag, an error
message) can read it inside the sandbox. `/usr/local/bin/claude` is a symlink to
`/usr/local/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe`, a single
native executable of a few hundred MB. Try `claude --help` first.

Search it for a **fixed string**, and cut the context out by byte offset. Run
each line as its own Bash call with the path written out: a shell variable set
and used in one sandbox Bash call can expand empty.

```bash
grep -a -b -o -F 'dangerously-skip-permissions' /usr/local/bin/claude | head   # OFFSET:match, under a second
tail -c +92625002 /usr/local/bin/claude | head -c 600 | tr -c '[:print:]\n' '.' # 600 bytes from 200 before OFFSET 92625201
strings -n 8 /usr/local/bin/claude | grep -F 'dangerously-skip-permissions'     # whole printable lines, a few seconds
```

`grep -b` counts from 0 and `tail -c +K` from 1, so `tail` takes `OFFSET-199`
to start 200 bytes before the match. `dd if=/usr/local/bin/claude bs=1
skip=<OFFSET-200> count=600 2>/dev/null` cuts the same window.

**Never** put a wildcard context pattern such as `grep -a -o '.{500}literal.{500}'`
on that file. With `-o` and a bounded repeat in front of the literal, grep tries
the match from nearly every byte offset: one such call has taken over six minutes
where the fixed-string search takes under a second, and the Bash tool shows
nothing until it returns (#310).

## Checking a hook change against the pinned CLI

Every sandboxed session starts with the `--settings` JSON that
`internal/sandbox/reap.go` builds: the SessionStart hook that installs the
interactive-git guard, and the PreToolUse reaper and guard. The exact value the
launcher passes is kept, one line, in `internal/sandbox/testdata/session-settings.json`.
A test fails when it goes stale, so after changing a hook, rewrite it first by
running that test with `-update`:

```bash
go test -buildvcs=false ./internal/sandbox/ -run TestTheSessionSettingsGoldenIsTheLaunchersSettings -update
```

Commit the rewritten file with the change; review then sees the settings diff.
Do not write a scratch `_test.go` into the package to print the JSON: the
commit step is `git add -A`, so one left behind gets committed (#437).

Then copy it out of the worktree and run the pinned CLI on it from a scratch
directory, so the run does not also load the worktree's own project settings:

```bash
mkdir -p /tmp/hook-check && cp internal/sandbox/testdata/session-settings.json /tmp/hook-check/
cd /tmp/hook-check
claude --settings "$(cat session-settings.json)" --help
claude -p hi --settings "$(cat session-settings.json)" --output-format stream-json --verbose
```

The first exits 1 on a value the CLI can't parse. The second prints a
`hook_started` and a `hook_response` event for each SessionStart hook before
`init`, with the hook's `exit_code`, `stdout` and `stderr`, even though the run
then stops on `Not logged in` because the sandbox has no credentials. The
PreToolUse hooks never fire there, since no tool is called: feed them a
`{"tool_name":"Bash","tool_input":{"command":"…"}}` event on stdin under `sh -c`,
as `internal/sandbox/reap_test.go` does. The SessionStart hook writes
`/tmp/agent-harness/interactive-git-guard.sh`, the same file your own session's
guard runs, so a changed guard applies to your session from then on.

## See also

- [`image`, `dockerfile` and `[cache]`](../reference/config.md)
- `agent-harness doctor` checks the sandbox image along with the rest of the setup.
