# ADR-0044: A sandboxed session reaps a leftover browser before a story run

- Status: Accepted
- Date: 2026-11-07
- Extends: ADR-0008 (what the harness puts in every sandbox)

## Context

On 2026-11-07 a herd pr-doctor CI-fix session for PR-1384 had to reproduce a
red Storybook story (#248). Its first run of the story file took 48 s and
reproduced the failure. After its fix, the second run took 165 s and failed with
`The browser did not close within 30000ms`. It wrapped the third run in
`timeout 500`, which printed only the file-selection line and was killed at
500 s. The fourth went to the background with a poll loop. The 25-minute
session cap then killed the session with nothing committed.

Two causes were confirmed on the host. The fix made the story hang, with the
headless Chromium renderer at about 115% CPU for over 8 minutes, while the
file's other stories ran green in 7 s. And the outer `timeout` killed pnpm but
not the browser, which slowed or blocked the next run. The Playwright and Chrome
DevTools MCP servers the project declares idled in the sandbox all session,
although a CI fix never drives a browser.

The session runs its own test commands, so the harness cannot clear the way
from the host. The base image does not promise `procps`, and a base-image change
reaches a Consumer only when it bumps its pinned tag (ADR-0008).

## Decision

**Every sandboxed session carries a PreToolUse hook that kills leftover headless
browsers and vitest processes before a story run, and a CI-fix session starts
with no MCP servers.**

1. **The hook travels in `--settings` on the claude command line**, not in the
   image, so it reaches the next session of every Consumer without a base-image
   release, and it adds to the Consumer's project settings rather than replacing
   them. It fires on a Bash command that names `vitest` or `storybook`.
2. **It matches a process by what it runs, not by what it mentions.** It reads
   argv[0] and argv[1] from `/proc` and kills a process whose argv[0] is
   `chrome-headless-shell` (or `headless_shell`), or whose argv[0] or argv[1]
   names vitest. `pkill -f` would also match the claude process, whose prompt
   names both. It is POSIX sh, because Claude Code picks the shell a hook runs in.
3. **A CI-fix session passes `--strict-mcp-config` with no `--mcp-config`**, so it
   starts none of the MCP servers the project declares. The other stages keep
   them, because a review or implementation session may verify a change in a
   browser. `hostio.AgentRun.NoMCP` is the per-session switch.
4. **The CI-fix envelope says how to run a browser-backed test:** the failing
   file once, in the foreground, with no outer `timeout` and no poll loop;
   commit the candidate fix before the run that verifies it, so the cap cannot
   throw it away; and treat a run that makes no progress for a few minutes as a
   failed fix, not a slow run. The escape hatch that said "only commit a
   locally-verified fix" now says "only commit a fix for a failure you actually
   reproduced", since the candidate commit comes before the verification.

## Consequences

- A vitest or browser run a session started in the background is killed by its
  next story run. The envelope already tells a CI-fix session not to start one.
- The hook names a node toolchain's processes in the harness core. A Consumer
  with neither pays one `jq` and `grep` per Bash call.
- Gate containers need no hook: each runs one command and is removed.
