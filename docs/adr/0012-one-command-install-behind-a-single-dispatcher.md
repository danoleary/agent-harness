# ADR-0012: One-command install, behind a single `agent-harness` entrypoint

- Status: Accepted
- Date: 2026-09-20

## Context

The harness ships as six binaries named for their role — `implementation`,
`review`, `retrospective`, `pipeline`, `loop`, `watch` — plus the `loop-start.sh`
launcher (ADR-0004, ADR-0005). Binaries are the primary distribution path
(ADR-0007): the target Consumer is not necessarily a Go shop and must not need a
toolchain.

Getting from "found the repo" to "ran a ticket" took an operator through: read the
prerequisites, find the Releases page, pick the archive matching their OS and
architecture, unpack it, decide where the binaries should live, move six of them
there, make sure that directory is on `PATH`, then find `.env.example` and
`CONSUMER.md` — which, until recently, were not even in the archive. Three of the
documented paths (`go install` six packages, `make build` from a checkout, the
archive) produced three different layouts, and `loop-start.sh` had already grown a
search across them because the launcher shipped *inside* the archive could not find
the binary shipped *beside* it.

The instruction that ended that sequence — "put the binaries on your `PATH`" — is
also a change to the operator's machine that nobody would agree to if it were
spelled out. `watch` shadows procps' `watch(1)`. `review`, `loop`, `pipeline`,
`implementation` are ordinary words that any project, any dotfile, any other tool
might already own. A distribution whose install step silently shadows a standard
Unix command is not one a careful operator should run, and "install this in a
single command" is not something we could honestly offer while that was the
install.

## Decision

**One command installs the harness, and exactly one name goes on `PATH`.**

```bash
curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh
```

`install.sh` resolves the latest release, downloads the archive for the host,
verifies it against the published `checksums.txt`, and lays it out under a prefix
it can write **without sudo** (`~/.local` by default):

```
<prefix>/bin/agent-harness            the dispatcher — the only thing added to PATH
<prefix>/libexec/agent-harness/       the six stage binaries + loop-start.sh
<prefix>/share/agent-harness/         .env.example, README.md, CONSUMER.md, LICENSE
```

`cmd/agent-harness` is a dispatcher: `agent-harness pipeline BEH-362` resolves the
`pipeline` binary out of that private `libexec` dir and **`syscall.Exec`s** it. The
stage binaries keep their names, their flags and their behaviour; they simply stop
being things an operator's shell can reach by accident.

Three properties of that dispatch are load-bearing:

- **`syscall.Exec`, not a child process.** The stage binary *replaces* the
  dispatcher, so the TTY, the signal disposition and the exit code are exactly what
  they were before. `loop` reads a first Ctrl-C as "stop after this ticket" and a
  second as a hard abort (ADR-0004), and `watch` renders its dashboard only on a
  real TTY (ADR-0005). A forwarding parent would have to reimplement both, and
  would get them subtly wrong.
- **`PATH` is never searched for a subcommand.** A `PATH` fallback for a
  subcommand named `watch` is the collision this ADR exists to prevent, arriving by
  a different door. The dispatcher searches the install's own directories — plus
  `$HARNESS_LIBEXEC` — and otherwise fails, naming every directory it looked in.
- **The archive layout *is* the installed layout.** `install.sh` copies it across
  rather than rearranging it, so an unpacked archive runs in place, and a release
  that drifts from what the installer expects fails in the release workflow rather
  than in every new operator's terminal.

`agent-harness start` is `loop-start.sh` reached through the same dispatcher, with
`LOOP_BIN` handed to it by the dispatcher that already knows which install it
belongs to.

## Alternatives considered

- **An installer that keeps the six names on `PATH`.** One command, same
  collisions. The one-liner would make the damage *easier* to do. Rejected.
- **Prefixed binaries (`agent-harness-loop`, …).** Fixes the collisions with no new
  code, but leaves six entries on `PATH`, six names to remember, and no single
  `--help` that shows an operator what the tool can do. Rejected as strictly worse
  than one name for the same fix.
- **Fold the six mains into one binary with subcommand packages.** The honest
  refactor, and where this may go eventually. It means moving ~1000 lines of
  working entrypoint code — including the daemon's signal handling — for no
  behaviour change. Rejected *for now*: the dispatcher gets the same operator-facing
  result without touching the code that runs unattended for hours. The dispatch
  table is the seam to do it behind later.
- **Homebrew / apt / a package manager.** Real packaging is better than `curl | sh`
  and is not exclusive with it, but every one of them is a per-platform pipeline to
  maintain, and none is a single command that works on a fresh macOS *and* a fresh
  Linux host today. Deferred, not rejected.
- **`sudo` into `/usr/local/bin`.** Already on `PATH` everywhere, so no follow-up
  step. Rejected: a script piped into a shell should not ask for root, and nothing
  the harness installs needs it.

## Consequences

- The documented commands are `agent-harness <command>`. The `make` targets are
  unchanged for from-source work, and `make install` now lays out the same prefix
  the installer does.
- The release archive moves the stage binaries to `libexec/agent-harness/`. An
  operator upgrading from a pre-0.4 archive by hand has six stale binaries wherever
  they put them; `install.sh --uninstall` does not know about those, since it
  removes only what it installs.
- `go install` remains available for Go shops and remains flat — it installs
  whatever you name into `GOBIN`, collisions included. `install.sh` is the
  recommended path precisely because it does not.
- The installer is a supported surface with its own tests
  (`scripts/test-install.sh`), which drive it against a fixture release served over
  `file://` — offline, and asserting the real dispatcher can resolve the layout the
  real installer produces.
- `install.sh --uninstall` removes the install file by named file, never
  `rm -rf` on a directory derived from an environment variable. A shared prefix like
  `~/.local` keeps everything the operator put there.
