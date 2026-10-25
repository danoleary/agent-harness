# Error messages and exit codes reference

The messages an operator is most likely to meet, by where they come from, with
what causes each and what to do. A fragment in `code` is the stable part of the
message to search for. Every setup error ends with
``Run `agent-harness doctor` for a full check.``

## Exit codes

| Command | `0` | `1` | `2` | `130` |
|---|---|---|---|---|
| `loop` | a deliberate stop: STOP, a `LOOP_MAX_*` ceiling, the circuit breaker; also `--help`, `--list`, `--dry-run` | a config, tracker or credential error at startup | a bad argument, an unknown loop name, an invalid instance id | a second Ctrl-C: hard abort, containers killed |
| `pipeline` | every Stage that ran succeeded; or `--next` found an empty queue | anything else, including a bad argument | — | — |
| `implementation`, `review`, `retrospective` | the Stage did its job | anything else | — | — |
| `start` | launched | already running, or no `loop` binary | a bad argument | — |
| `doctor` | every check passed | a check failed | a bad argument | — |
| `init` | done | scaffolding failed | a bad flag or argument | — |

Nothing restarts a stopped loop. The `loop` exit code tells a deliberate stop
(`0`) from a crash, so a person looks before more tickets are spent.

## Environment and credentials

| Message | Cause | Fix |
|---|---|---|
| `the harness environment has a problem:` / `has N problems:` followed by the list, then `Searched for a .env file in this order` | a required variable is unset, or set in a file the harness didn't read | put it in the first file listed, or export it ([env file order](env.md#where-the-harness-reads-them)) |
| `no Claude credential is set` | neither Claude variable is set | [set one](../how-to/claude-credential.md) |
| `ANTHROPIC_API_KEY holds a Claude subscription token (sk-ant-oat01-…)` | a subscription token in the API-key variable | move it to `CLAUDE_CODE_OAUTH_TOKEN` |
| `Invalid API key`, in a session transcript | the same, past `doctor` | the same |
| `GH_TOKEN is not set` | — | create a [classic `repo` token](tokens.md#gh_token) |
| `PROJECT_PATH is not set.` / `a relative path` / `does not exist` / `is a file` | — | an absolute path to the checkout directory |
| `startup GitHub check: … GH_TOKEN was rejected (expired or revoked?)` | the loop's first GitHub read got HTTP 401 | replace the token |
| `the <kind> tracker rejected a queue read` | the tracker credential or config is wrong | `agent-harness doctor` |
| `LINEAR_API_KEY is not set` / `JIRA_… not set` | `kind` needs a credential that is missing | [Linear](../how-to/linear.md), [Jira](../how-to/jira.md) |
| `GH_TOKEN cannot read check runs` (warning) | a fine-grained or non-SSO-authorized token | the PR still ships, unwatched; use a classic `repo` token |

## Project config

| Message | Cause | Fix |
|---|---|---|
| `has no .agent-harness/ directory` | the project was never scaffolded | `agent-harness init` |
| `parsing project config <path>:<line>:<col>` | a TOML syntax error | fix it at that position |
| `invalid project config <path> (N problems):` | the listed validation problems | fix each one; see [config reference](config.md) |
| `a sandbox image source is required` | neither `image` nor `dockerfile` | set one |
| `at least one [[gates]] entry is required` | no gates | add one |
| `tracker.team_key is required for kind=linear` / `tracker.ready_label is required for kind=linear` | — | set them |
| `tracker.repo must be "owner/name"` | — | set `repo` |
| `warning: … unknown key <name>, ignored` | a misspelled key, or one from a newer harness | fix the spelling, or upgrade |
| `missing or blank prompt body:` | a prompt body is absent or empty | [write the prompt bodies](../how-to/prompt-bodies.md) |
| `this project requires agent-harness >= X` | `min_harness_version` is newer than the binary | upgrade |
| `SESSION_IDLE_TIMEOUT (…) must be below the … cap` | the idle timeout can never fire | lower it, or raise the cap |
| `<KEY> must be <want>, got "<raw>"` | a bad `LOOP_*`, `GATE_*` or `PR_LOOP_*` value | [env reference](env.md#the-loop-daemon) |
| `loops[i] ("name"): …` | a `[[loops]]` problem: an unknown step, a step out of order, a missing prompt | [declare a loop](../how-to/declare-a-loop.md) |
| `no transition configured for issue PROJ-1` / `transition "…" not available on issue PROJ-1` | a Jira transition key is missing or misspelled | [Jira](../how-to/jira.md#2-find-your-transition-names) |

## Sandbox preflight

A preflight failure ends the run **PreflightAborted**. The ticket goes back to
Todo, the circuit breaker doesn't count it, and the loop reclaims disk and backs
off.

| Message | Fix |
|---|---|
| `docker daemon not reachable — is Docker Desktop running?` | start Docker |
| `insufficient free disk to launch a sandbox` | free disk: `docker builder prune -af`, `docker system prune -af` |
| `sandbox image "…" missing and the build failed` | build it by hand with the command shown, and fix the Dockerfile |
| `sandbox image "…" missing and the pull failed` | check the ref and `docker login` |
| `is out of date with <Dockerfile> and the rebuild failed` | the same as a failed build |

## Interrupted sessions

`⏸ session interrupted by signal N` means a stop signal killed a session from
outside: exit 130 is SIGINT (a Ctrl-C), 143 is SIGTERM (a `docker stop`, or the
host shutting down). The run ends **Interrupted**, not failed. The harness skips
the handoff check and the retrospective, leaves the ticket In Progress with its
worktree for the next run, and the circuit breaker doesn't count it. Nothing to
fix unless you didn't send the signal: then look for what is stopping containers.
Exit 137 (SIGKILL) is not an interruption — it is an OOM-kill or the session cap.

## Worktree setup and gates

| Message | Cause | Fix |
|---|---|---|
| ``could not execute `…` (exit 127)`` or `(exit 126)`, `the command is not on PATH in the worktree-command container` | `post_create` names a tool the image's `PATH` lacks | put it on `PATH` with `ENV` in the Dockerfile ([sandbox image](../how-to/sandbox-image.md#3-put-every-tool-on-the-image-path)) |
| `⚠ post_create exited N` | `post_create` failed some other way | a warning only; the session installs what's missing |
| `gate re-run could not execute … toolchain missing in the gate shell` | the same PATH problem, in a gate | the same |
| `harness gate re-run is red — not pushing` | a gate failed on the host | read the gate output in the review log |
| `review ✗ no worktree at … — run implementation first` | `review` was run alone | run `implementation` first |

## Landing

How a review ends when it takes the branch to a PR (`ship.Land`).

| Message | Verdict | What to do |
|---|---|---|
| `rebase collapsed the branch to zero net change against origin/main` | Collapsed | the work is already on `main`; close the ticket |
| `disjoint branch history — no common ancestor with origin/main` | Disjoint | reset the branch onto `origin/main` and cherry-pick the handoff commits |
| `conflict-resolution session left the worktree dirty` / `left the branch on its stale base` | Conflict | resolve the conflict by hand in the kept worktree |
| `review ✗ push failed:` | PushFailed | usually the token: scope, SSO, or `workflow` for a workflow-file change |
| `review ✗ gh pr create failed: … branch pushed, open the PR manually` | PRFailed | open the PR yourself |

## The loop

| Message | Meaning |
|---|---|
| `spending cap reached, retry after reset` | **CapAborted**: the ticket goes back to Todo and the loop backs off until the cap resets. Nothing to do. |
| `circuit breaker tripped: … failed N× — start here` | the same ticket failed every time; start with it |
| `circuit breaker tripped: N consecutive tickets failed to reach a pushed PR` | something environmental is failing every run; fix it and relaunch |
| `loop already running (pid …)` | a second `start` of a running instance; STOP it, or use `--instance` |
| `was claimed by another run first — selecting again` | not an error: another instance won the claim |
| `releasing … back to Todo failed — move it out of In Progress manually` | the tracker refused the release; move the ticket by hand |

For problems with no message of their own, such as a daemon that stalls on a Mac
or opaque bash errors in a transcript, see [troubleshooting](../troubleshooting.md).
