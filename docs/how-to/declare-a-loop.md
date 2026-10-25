# Change a loop or declare your own

A **loop** is one daemon: a **Source** it polls for work, and the **Steps** it
runs over each **Work item** the Source yields (ADR-0019). Two loops are built in:

| Name | Source | Steps | Runs with |
|---|---|---|---|
| `tickets` | `tracker`: the ready queue | `implement`, `review`, `retro` | `agent-harness loop` (the default) |
| `pr-doctor` | `github_prs`: open PRs that conflict with `main` or have red CI | `checkout_pr`, `fix`, `gates`, `push_existing`, `watch_ci`, `retro` | `agent-harness loop pr-doctor` |

## See what a project can run

```bash
agent-harness loop --list                # every loop this project can run
agent-harness loop pr-doctor --dry-run   # its Source, Steps and limits as resolved, and the PRs it would work; changes nothing
agent-harness loop pr-doctor             # run it in the foreground
```

## Override a built-in loop, or add one

Add a `[[loops]]` entry to `.agent-harness/config.toml`. An entry with a
built-in's name replaces it; any other name adds a loop. This one re-tunes
`pr-doctor` and gives its fix step a project body:

```toml
[[loops]]
name = "pr-doctor"
poll_interval = "10m"            # each limit overrides its LOOP_* env knob, for this loop only
max_consecutive_failures = 3
source = { kind = "github_prs", label = "agent-maintained", when = ["conflicting", "ci_failing"] }
steps = [
  { use = "checkout_pr" },
  { use = "fix", prompt = "prompts/fix-pr.md" },   # a Project body, relative to .agent-harness/
  { use = "gates" },
  { use = "push_existing" },
  { use = "watch_ci" },
  { use = "retro" },
]
```

Then check it loads and does what you expect:

```bash
agent-harness doctor --offline
agent-harness loop pr-doctor --dry-run
```

The config load reports every problem at once, each at its
`config.toml:line:column`: an unknown source or step, a step out of order, a
prompt file that is missing or blank.

The Sources and Steps are a fixed set, and each is Go code that checks its own
result on the host before the next Step runs. Config chooses them, orders them,
sets their parameters and gives a Step a prompt body. It cannot add a Step that
runs a shell command, or an agent with nothing checking what it did (ADR-0016).
Every key, Source and Step, and the order Steps must come in, is listed under
[`[[loops]]`](../reference/config.md#loops) in the config reference.

## Stop and watch a loop

Each loop has its own sentinel, `.agent-harness/STOP-<name>`; `touch` it to wind
that loop down. The plain `.agent-harness/STOP` (`STOP_FILE`) winds down every
loop. A loop clears both when it starts, unless another daemon `agent-harness
start` launched is still running: a STOP that daemon has not reached yet stays
for it.

The `tickets` loop logs to `.agent-harness/logs/`. Every other loop logs to
`.agent-harness/logs/loops/<name>/`, one dir per item plus its own `loop.jsonl`.
Start one as a detached daemon, and follow it, with:

```bash
agent-harness start <name>        # pidfile .agent-harness/loop-<name>.pid, log loop-<name>.log
agent-harness watch --loop <name>
```

The viewer shows the daemon as down when `loop-<name>.pid` names no live
process, so a loop run in the foreground with `agent-harness loop <name>` reads
as down; and it shows "winding down" once `STOP-<name>` or the plain `STOP` is
present.

Run each loop in its own daemon. The ticket loop may run as
[several instances](run-several-instances.md); every other loop runs as one
daemon per checkout.
