# Run more than one instance of the ticket loop

A deep backlog of independent tickets goes faster with two ticket-loop daemons
against the same checkout. Each is an **instance**, named by an id (ADR-0024).

## Start, watch and stop them

```bash
agent-harness start                  # the default instance
agent-harness start --instance b     # a second one, beside it
agent-harness watch --instance b     # follow b alone
agent-harness watch --all            # follow every instance, each line labeled [default], [b], …
touch .agent-harness/STOP-b          # stop b after its current ticket
touch .agent-harness/STOP-tickets    # stop every instance of the ticket loop
touch .agent-harness/STOP            # stop every loop
```

The default instance has no sentinel of its own. To be able to stop either of two
instances alone, name both: `start --instance a` and `start --instance b`.

An id is lowercase letters, digits and dashes, and may not be a loop's name or
`default`. A second `start` of a running instance refuses. In the foreground the
same flag works: `agent-harness loop --instance b`. Only the ticket loop runs as
several instances; every other loop runs as one daemon per checkout.

## Where each instance keeps its files

| | Default instance | Instance `<id>` |
|---|---|---|
| pidfile | `.agent-harness/loop.pid` | `.agent-harness/loop-<id>.pid` |
| daemon log | `.agent-harness/loop.log` | `.agent-harness/loop-<id>.log` |
| own sentinel | none: `STOP-tickets` stops every instance | `.agent-harness/STOP-<id>` |
| event stream and per-ticket logs | `.agent-harness/logs/` | `.agent-harness/logs/instances/<id>/` |

Add the runtime files to your `.gitignore` (`agent-harness init` does):
`.agent-harness/STOP-*`, `.agent-harness/loop-*.log`, `.agent-harness/loop-*.pid`
and `.agent-harness/locks/`.

## What the instances share

- **The claim.** Instances select from the same queue, so two can pick the same
  ticket at the same moment. They claim under one lock
  (`.agent-harness/locks/claim.lock`), and each re-reads the ticket before it
  claims it: Linear checks the state is still unstarted, GitHub that the issue
  still has the ready label and not the in-progress one, and Jira that the
  claim transition would still move it. An instance that loses selects again; it
  claimed nothing, so it releases nothing. Every instance must run on the same
  host, because the lock is a file.
- **The gates.** Each run executes its Gates in a container, and two gate sets at
  once can overload a laptop (exit-137 OOM kills). `GATE_CONCURRENCY` (default 1)
  is how many runs may gate at once; a run past it implements and reviews as
  usual and waits only for its gate step, saying so in the log. `0` lifts the
  limit. The slots are lock files under `.agent-harness/locks/`; set
  `GATE_LOCK_DIR` to one directory in several projects' env to bound the whole
  host.
- **The reaper.** Each instance reaps stale claims between its own tickets. It
  never releases another instance's healthy claim: a claim younger than the TTL,
  or one with a pushed branch or a linked PR, is left alone.

## See also

- [`agent-harness start`, `loop` and `watch`](../reference/cli.md)
- [`GATE_CONCURRENCY`, `GATE_LOCK_DIR` and the `LOOP_*` knobs](../reference/env.md)
