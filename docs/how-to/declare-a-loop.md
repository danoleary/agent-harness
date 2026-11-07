# Change a loop or declare your own

A **loop** is one daemon: a **Source** it polls for work, and the **Steps** it
runs over each **Work item** the Source yields (ADR-0019). Three loops are built in:

| Name | Source | Steps | Runs with |
|---|---|---|---|
| `tickets` | `tracker`: the ready queue | `implement`, `review`, `watch_ci`, `retro` | `agent-harness loop` (the default) |
| `pr-doctor` | `github_prs`: open PRs that conflict with `main` or have red CI | `checkout_pr`, `fix`, `gates`, `push_existing`, `watch_ci`, `retro` | `agent-harness loop pr-doctor` |
| `triage` | `backlog`: tickets not yet labelled ready for an agent | `assess`, `apply` | `agent-harness loop triage` |

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
source = { kind = "github_prs", scope = "all", when = ["conflicting", "ci_failing"] }   # "opted_in" (or a label) limits it to branch_prefix and labelled PRs
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

## Stop the ticket loop waiting on CI

The built-in `tickets` loop watches each PR's CI to a verdict before the
retrospective runs and the next ticket starts, and fixes a red check itself. That
watch is often most of the review stage. If you run `pr-doctor` beside it, leave
`watch_ci` out of the ticket loop; the review stage then stops once the PR is
open, and `pr-doctor` fixes a check that goes red later (ADR-0031):

```toml
[[loops]]
name = "tickets"
source = { kind = "tracker" }
steps = [{ use = "implement" }, { use = "review" }, { use = "retro" }]
```

Without a running PR loop, red CI on those PRs then waits for a human.

## Triage the backlog into the ready queue

The `triage` loop judges, one per poll, each open and unassigned ticket that
carries neither `ready_label` nor `blocked_label` (ADR-0044). A read-only session
reads the ticket against the code and leaves a verdict:

- **Ready**: the ticket gets `ready_label`, and on Linear moves from Backlog to
  Todo, so the `tickets` loop picks it up. A short comment says why.
- **Not ready**: the ticket gets one comment listing what a human must answer or
  do first. Edit the description to answer, and triage judges it again; or add
  `ready_label` yourself.

A judged ticket is not judged again until its title or description changes. The
verdicts are kept in `.agent-harness/locks/triage/`. It runs on Linear and GitHub
Issues; Jira's ready queue is a JQL query the harness cannot add a ticket to.

Try it without touching the tracker first: leave out `apply`, and it only records
verdicts (in its `loop.jsonl` and per-ticket logs). Give `assess` your project's
own bar for "ready", which leads the harness's default one:

```toml
[[loops]]
name = "triage"
poll_interval = "15m"
source = { kind = "backlog" }
steps = [{ use = "assess", prompt = "prompts/triage.md" }]   # add { use = "apply" } once the verdicts look right
```

```bash
agent-harness loop triage --dry-run   # the tickets it would judge, in order
agent-harness start triage            # run it beside the ticket loop
```

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
