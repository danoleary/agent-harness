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
| `gates[i] ("name") cache_isolate path "…" is empty` / `is absolute` / `is the worktree itself` / `leaves the worktree` / `contains a comma` | a `cache_isolate` path that does not name a directory inside the worktree, so the gate's tmpfs could not be mounted safely | make it a worktree-relative directory such as `web/node_modules/.vite`; see [config reference](config.md#gates) |
| `gates[i] ("name") skip_when_only pattern "…" is empty` / `is absolute` / `is not a valid glob` | a `skip_when_only` glob that could never match a repo-relative path | fix the glob; see [config reference](config.md#gates) |
| `sandbox.shm_size "…" is not a size` | a `[sandbox] shm_size` docker's `--shm-size` would not take: no number, zero, a space, or a unit other than `b`, `k`, `m`, `g` | write it as e.g. `"2g"` or `"512m"`; see [config reference](config.md#sandbox) |
| `tracker.team_key is required for kind=linear` / `tracker.ready_label is required for kind=linear` | — | set them |
| `tracker.repo must be "owner/name"` | — | set `repo` |
| `warning: … unknown key <name>, ignored` | a misspelled key, or one from a newer harness | fix the spelling, or upgrade |
| `missing or blank prompt body:` | a prompt body is absent or empty | [write the prompt bodies](../how-to/prompt-bodies.md) |
| `⚠ <session>: prompt body names {{.X}}, which this harness does not know` | the body uses a field the running binary lacks: a body newer than the binary, or a typo. A warning only; the field renders empty and the rest still render | fix the spelling, or upgrade ([template fields](../how-to/prompt-bodies.md#3-fill-in-the-template-fields)) |
| `⚠ <session>: prompt body was not rendered, so it goes out with its placeholders as literal text (…)` | the body does not parse as a Go `text/template`. A warning only; the session runs on the raw body | fix the template syntax at the position the error names |
| `⚠ retrospective: no transcripts under …` | the run left no session transcripts in its log dir. A warning only | nothing, unless a session should have run: then read `run.jsonl` |
| `this project requires agent-harness >= X` | `min_harness_version` is newer than the binary | upgrade |
| `SESSION_IDLE_TIMEOUT (…) must be below the … cap` | the idle timeout can never fire | lower it, or raise the cap |
| `<KEY> must be <want>, got "<raw>"` | a bad `LOOP_*`, `GATE_*` or `PR_LOOP_*` value | [env reference](env.md#the-loop-daemon) |
| `loops[i] ("name"): …` | a `[[loops]]` problem: an unknown step, a step out of order, a missing prompt | [declare a loop](../how-to/declare-a-loop.md) |
| `no transition configured for issue PROJ-1` / `transition "…" not available on issue PROJ-1` | a Jira transition name is misspelled (a missing key fails at config load) | [Jira](../how-to/jira.md#2-find-your-transition-names) |

## Sandbox preflight

A preflight failure ends the run **PreflightAborted**. The ticket goes back to
Todo (a PR keeps its attempt: the loop takes it back), the circuit breaker
doesn't count it, and the loop reclaims disk and backs off: one poll interval,
doubling with each abort in a row up to 16. The `ticket-released` record in
`loop.jsonl` carries the message below in `detail`.

| Message | Fix |
|---|---|
| `docker daemon not reachable — is Docker Desktop running?` | start Docker |
| `insufficient free disk to launch a sandbox` | free disk: `docker builder prune -af`, `docker system prune -af` |
| `sandbox image "…" missing and the build failed` | build it by hand with the command shown, and fix the Dockerfile |
| `sandbox image "…" missing and the pull failed` | check the ref and `docker login` |
| `is out of date with <Dockerfile> and the rebuild failed` | the same as a failed build |

Once the image is ready, a session's own launch can still go wrong:

| Message | Cause | Fix |
|---|---|---|
| `session ✗ docker could not start the container (exit 125): …` | docker refused the `docker run`. The reason follows the colon. Without one, it names the log that has docker's error: an agent session's `<session>-<run-id>.stderr.log`, or a step's `.log` | what docker's error says. A transient overlay2 or read-only-fs failure is retried on its own |
| `Ignoring N permissions.allow entries from .claude/settings.json: this workspace has not been trusted`, in a session's `.stderr.log` | the sandbox image is built on a base image released before #305, whose entrypoint does not trust the session's working directory. Harmless: the session runs with `--dangerously-skip-permissions`, and stderr stays out of the `.jsonl` transcript | bump the base image tag your Dockerfile pins to a release that has #305 |

## Interrupted sessions

`⏸ session interrupted by signal N` means a stop signal killed a session from
outside: exit 130 is SIGINT (a Ctrl-C), 143 is SIGTERM (a `docker stop`, or the
host shutting down). The run ends **Interrupted**, not failed. The harness skips
the handoff check and the retrospective, leaves the ticket In Progress with its
worktree for the next run, and the circuit breaker doesn't count it. Nothing to
fix unless you didn't send the signal: then look for what is stopping containers.
Exit 137 (SIGKILL) is not an interruption — it is an OOM-kill or the session cap.

## A red base

`⏸ base <sha> fails gate "…" before any change — holding <KEY>` means `main`
itself fails a gate. After `post_create`, the implementation stage runs the gates
over the untouched base, keyed by its SHA. A red run is re-run once
(`basegate-confirm-<name>`), and a pass there records the base green and
dispatches, logged as `base gate "…" flaked`. If they fail twice, no session
launches: the ticket goes back to Todo, its empty worktree and branch are
removed, and the run ends **BaseRed** (ADR-0036). Every ticket cut from the same
base is held without gating it again. The circuit breaker doesn't count it. The
harness files one finding, `base-gates-red`, for a human.

`loop ⏸ still held on <sha>, re-check at HH:MMZ` then follows once a poll: the
loop refetches `main` and, while it is still on the held base and the red verdict
has not expired, selects nothing, so no claim, worktree or `post_create` is spent
(ADR-0039). It selects again on the first poll after `main` moves, or at the
re-check time.

Fix: repair `main`. Or dispatch the finding's issue by hand
(`agent-harness pipeline <KEY>`), since an issue carrying that finding key is
exempt from the hold: its base is still gated, but a red one logs
`base <sha> is red, as <KEY> reports — dispatching the repair` and launches the
session. If the base is green by then, the run logs `base <sha> that <KEY>
repairs is already green` and the session's envelope says so, so it adds only
the regression test or hardening the issue asks for (#278). Close the issue
once that lands. `--force` also skips the check. A red verdict is trusted
for 30 minutes, so a gate that flaked on both runs is gated again after that. The verdicts live under
`.agent-harness/locks/base-gates/`; delete one to re-gate its base now. A
running loop keeps idling on the hold it recorded until the re-check time, so
restart it too.

Base gating fails open: when the harness can't gate the base, or can't record
what it found, it warns and dispatches rather than hold a ticket on a guess.

| Message | Cause | Fix |
|---|---|---|
| `⚠ could not read the base SHA (…) — dispatching without gating the base` | git could not read the fresh worktree's `HEAD` | nothing for this ticket; if it repeats, check the worktree with `git -C <path> rev-parse HEAD` |
| `⚠ base gate "…" failed environmentally (exit N: an OOM-kill, a failed launch or its cap)` | the gate was killed or never started, so its red says nothing about the base. The PR loop and pr-fix log the same with their own prefix and keep the red re-gate as the branch's | nothing, unless it repeats: then give Docker more memory, or raise the gate cap |
| `⚠ base gate "…" failed environmentally on its re-run (exit N) — the red is unconfirmed` | the first run was red, and the confirming re-run was killed or never started | the same |
| `⚠ could not record the base verdict (…)` | the verdict file under `.agent-harness/locks/base-gates/` could not be written | nothing urgent: the next ticket (or PR loop run) on this base gates it again. Check that directory is writable |
| `⚠ could not write the red-base finding (…)` | the `base-gates-red` finding could not be written to the findings dropbox, so no issue is filed for the red base | the hold still applies; repair `main` yourself, and check the run's log dir is writable |
| `⚠ could not discard the untouched worktree on the red base (…)` | the held ticket's worktree or branch could not be removed | remove it with `git worktree remove` and `git branch -D`, or the next attempt reuses it at the old base |

## Worktree setup and gates

| Message | Cause | Fix |
|---|---|---|
| ``could not execute `…` (exit 127)`` or `(exit 126)`, `the command is not on PATH in the worktree-command container` | `post_create` names a tool the image's `PATH` lacks | put it on `PATH` with `ENV` in the Dockerfile ([sandbox image](../how-to/sandbox-image.md#3-put-every-tool-on-the-image-path)) |
| `fast-forwarded … from … to … — it had no commits ahead, so it now starts from current origin/main` | a reused worktree's branch held no work and sat on a base origin/main has moved past, so the harness reset it onto current origin/main before `post_create` and the base gate (#334) | nothing; a branch with commits ahead, or a tree with uncommitted work, is left where it is |
| `⚠ could not fast-forward the reused worktree onto origin/main (…)` | git could not read or reset the reused worktree | the session runs on its old base; if it repeats, check the worktree with `git -C <path> status` |
| `⚠ post_create exited N` | `post_create` failed some other way | a warning only; the session installs what's missing |
| `gate re-run could not execute … toolchain missing in the gate shell` | the same PATH problem, in a gate | the same |
| `harness gate re-run is red — not pushing` | a gate failed on the host | read the gate output in the review log |
| `↻ gate "…" is red (exit N) with …: infra — re-running it once` | the gate's log shows an environment failure (the test browser's connection closed or its session never opened, a test timeout such as `Test timed out in 30000ms`, a Vite reload — `optimized dependencies changed`, `Failed to fetch dynamically imported module` — an out-of-memory kill or SIGKILL, a download failure, the gate cap), or a PR loop CI fix's gate failed only in files the fix did not touch | nothing; only a second red counts. If the re-run is red too, read its log (`gate-…-rerun-*.log`) |
| `✗ gate "…" is red (exit N): test failure` or `✗ gate "…" is red again on its re-run (exit N): test failure` | the gate failed with no environment signature (a real assertion), or it was re-run over one and failed again; either red is final (#312) | read the gate's log (`gate-…-*.log`, or `gate-…-rerun-*.log` after a re-run) |
| `✗ gate "…" is red over auto-fix N merged with origin/main, though green on the branch alone — not pushing` | a CI fix passes the gates on its branch but not merged with origin/main, which is what CI checks (#259) | nothing; the next CI fix attempt gets that gate's output (`gate-cifixN-merge-*.log`). A fix that conflicts with origin/main is refused too; the PR loop's next poll resolves the conflict |
| `pr-fix · gate "…" skipped — every path the fix changes against origin/main matches its skip_when_only (…); CI checks it before merge` | a PR loop fix touched only paths that gate's `skip_when_only` lists, so it was not run host-side (#221) | nothing; CI runs the check before merge. To gate such a fix locally, narrow or drop the gate's `skip_when_only` |
| `tdd ≡ already resolved on main by <sha> — recommend close: <reason>` | **ResolvedOnMain**: the session found the ticket already fixed by that `origin/main` commit and stopped without committing, and git confirmed `main` has it (ADR-0052). Not a failure; the loop labels the ticket blocked and comments the reason | check the commit fixes the ticket and close it; if it does not, remove the blocked label and say on the ticket what is missing |
| `the session's resolved.json does not hold: …` | the session stopped without a handoff commit and wrote `/findings/resolved.json`, but its commit is not on `origin/main` (or the file is not `{commit, reason}` JSON). The run fails as a no-handoff run | read the session's transcript for the commit it meant; close the ticket by hand if it is fixed, or let a later run implement it |
| `review ✗ no worktree at … — run implementation first` | `review` was run alone | run `implementation` first |
| `⚠ could not strip <paths> from the worktree (…)` | after a green handoff, the `handoff_strip_paths` build artifacts could not be removed. A warning only; the handoff stands | the reviewer re-runs `post_create`; if its gates crash on a stale artifact, delete the paths in the worktree by hand |

## Dispatch advisories

Warnings logged as a ticket is dispatched. None of them stops the session; the
session's prompt tells it what to do about each.

| Message | Cause | Fix |
|---|---|---|
| `⚠ <KEY> still lists open questions for a human ("…") — dispatching anyway` | the ticket is ready-labelled but its body still has an open-questions section | settle them in the ticket, or drop the ready label if they block the work. The session records any it has to decide and flags them |
| `⚠ <KEY> cites symbol(s) absent from source: … — likely already resolved` | code identifiers the ticket names are not in the source tree, often because a sibling ticket's merge removed them | check the premise still holds; close the ticket if the work has landed |
| `⚠ <KEY> already has commit(s) on feat/<slug> ahead of origin/main referencing it` | a resumed worktree's branch already carries commits for this ticket, likely a complete fix that never reached a PR | review `git log origin/main..feat/<slug>` and the diff; if it is done, let the session verify and hand off, or open the PR |
| `⚠ <KEY> cites Sentry <ref> but it could not be fetched: <reason>` | the ticket links a Sentry issue the harness could not read, so the session runs without its stack trace | check the Sentry token and the link; the reason names which failed |

## Recovering a failed session

When a session fails and leaves uncommitted edits, the harness commits them as a
recovery checkpoint on the feature branch, so the next attempt resumes from them.

| Message | Cause | Fix |
|---|---|---|
| `⚠ uncommitted work remains in the worktree at <path> and the recovery checkpoint commit failed (…)` | the implementation session failed with a dirty worktree, and the checkpoint commit did not go through | commit or save the diff in that worktree by hand before re-running, or the next attempt may lose it |
| `⚠ review session left uncommitted edits and the recovery checkpoint commit failed (…)` | the same, for a review session's in-progress fixes | the same, at the path the line names |
| `⚠ disjoint branch detected but the regraft failed (…)` | the verified branch shares no history with `origin/main`, and re-grafting its diff onto a fresh base failed. The worktree is kept | reset the branch onto `origin/main` and cherry-pick the handoff commits by hand (as for a Disjoint landing) |

## Landing

How a review ends when it takes the branch to a PR (`ship.Land`).

| Message | Verdict | What to do |
|---|---|---|
| `rebase collapsed the branch to zero net change against origin/main` | Collapsed | the work is already on `main`; close the ticket |
| `disjoint branch history — no common ancestor with origin/main` | Disjoint | reset the branch onto `origin/main` and cherry-pick the handoff commits |
| `ship … replay onto origin/main failed with no content conflict` then `ship ↻ retrying the replay once` | — | nothing: git failed with nothing unmerged (a held `index.lock`, a file in the way), and the replay is run once more. git's output is in the line |
| `review ✗ pre-push rebase failed with no content conflict` | RebaseFailed | read git's output in the `ship …` lines of `run.jsonl`, fix what it names, and rebase the kept worktree onto `origin/main` by hand (#164) |
| `conflict-resolution session left the worktree dirty` / `left the branch on its stale base` | Conflict | resolve the conflict by hand in the kept worktree |
| `scoped review of the conflict resolution: …` | Conflict | the resolution changed files git did not flag, and their review did not clear them; read the breadcrumb's file list and the range-diff in the kept worktree (#114) |
| `⚑ the conflict resolution reports the branch duplicates what origin/main added in …` | — | nothing blocks: the PR ships with an `Overlap with main` section listing what the branch reimplements of what `main` gained while it was in flight. Decide on the PR whether the branch should switch to `main`'s version (#255) |
| `⚠ could not read the conflict resolution's overlap report (…)` | — | the session wrote `findings/rebasefix/overlaps.json` but not as a JSON array; the PR goes out without the overlap section. Read the file in the ticket's log dir (#255) |
| `⚠ could not prepare the conflict resolution's overlap dropbox (…)` | — | the host could not create or clear `findings/rebasefix/` under the ticket's log dir, so the session had nowhere to report an overlap; check that dir's permissions (#255) |
| `review ✗ push failed:` | PushFailed | usually the token: scope, SSO, or `workflow` for a workflow-file change |
| `review ✗ gh pr create failed: … branch pushed, open the PR manually` | PRFailed | open the PR yourself |

## The loop

| Message | Meaning |
|---|---|
| `spending cap reached, retry after reset` | **CapAborted**: the ticket goes back to Todo and the loop backs off until the cap resets. Nothing to do. |
| `circuit breaker tripped: … failed N× — start here` | the same ticket failed every time; start with it |
| `circuit breaker tripped: N consecutive tickets failed to reach a pushed PR` | something environmental is failing every run; fix it and relaunch |
| `loop already running (pid …)` | a second `start` of a running instance; STOP it, or use `--instance` |
| `already resolved on main by <sha>; handing it to a human to close` | **ResolvedOnMain**: the ticket is labelled blocked and left In Progress, with the commit and the session's reason in a comment. Breaker-neutral. Confirm and close it |
| `was claimed by another run first — selecting again` | not an error: another instance won the claim |
| `skipping <KEY>: last run found no work against origin/main <sha>; unchanged` | the ticket's last run ended with no commit ahead of `origin/main`, and neither main nor the ticket's title, body or labels have changed since, so selection passes over it (#330). Often the fix is already on main under another PR: close the ticket. To dispatch it anyway, edit the ticket, run `pipeline <KEY>` by hand, or delete `.agent-harness/locks/no-work/<slug>` |
| `queue held: N ticket(s) whose last run found no work on an unchanged main (…)` | every ready ticket was passed over as above; the loop idles rather than reporting an empty queue |
| `<KEY>'s last run found no work against origin/main <sha>; main is now <sha> — dispatching it again` / `the ticket changed since — dispatching it again` | not an error: the record no longer stands, so the ticket is selected as usual |
| `tdd · the session ran to completion with no work against origin/main <sha>` | the implementation session exited 0 and left nothing to hand off; the run is recorded so selection skips the ticket until main or the ticket moves |
| `⚠ could not record that the run found no work (…)` / `⚠ could not read origin/main to record that the run found no work (…)` | the record under `.agent-harness/locks/no-work/` could not be written. A warning only: the next poll may run the ticket once more, as before #330 |
| `⚠ could not fetch origin/main to check a ticket whose last run found no work (…)` | the fetch before comparing a skipped ticket's recorded base failed. A warning only: the screen compares against the last fetch, so the ticket may stay skipped until a fetch succeeds |
| `releasing … back to Todo failed — move it out of In Progress manually` | the tracker refused the release; move the ticket by hand |

For problems with no message of their own, such as a daemon that stalls on a Mac
or opaque bash errors in a transcript, see [troubleshooting](../troubleshooting.md).
