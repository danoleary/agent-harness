# ADR-0030: A signal-killed session is Interrupted, not failed

- Status: Accepted
- Date: 2026-10-25
- Amends: the "retrospective always runs" rule (DESIGN.md "The pipeline")

## Context

A Stage's session was killed from outside a couple of minutes in: exit 143,
SIGTERM. The harness took its ordinary failure path. It ran the handoff
verification, which logged "no handoff commit". It skipped review because
implementation "failed". Then it launched the retrospective, because the
retrospective always runs. That session was killed within seconds too, leaving a
stub transcript and no findings.

So the run log recorded an implementation failure that never happened. A human
or a later retrospective had to work out that the slice was interrupted, not
broken. And the harness spent a sandbox launch inside a process that was already
going down.

The session runner knew only two kinds of non-zero exit: Retryable (the 137
OOM-kill and a transient 125) and everything else.

## Decision

**A session killed by a stop signal is a third kind of exit, and a run that ends
in one has its own Disposition, Interrupted.**

- `session.Outcome.Interrupted()` reports the signal when the exit code is 128+N
  for SIGHUP, SIGINT, SIGQUIT or SIGTERM (129, 130, 131, 143). SIGKILL's 137 is
  left out: it is the OOM-kill and the watchdog's own `docker kill`, which have
  their own classes. A docker client that a signal killed has no exit code
  (`-1`); the runner reports it as 128+N, like a shell.
- Each Stage checks for it right after its session exits, in the shared frame
  (`frame.interrupted`). It logs "⏸ session interrupted by signal N" and returns
  `Result{Disposition: Interrupted, Signal: N}`. It does no ground-truth check, no
  checkpoint commit, no release and no findings filing. The worktree and the claim
  are left as the session left them.
- The pipeline stops at an Interrupted Stage. It runs no later Stage, not even the
  retrospective. This is the one exception to "the retrospective always runs". The
  fold places Interrupted after the other signals, so a review that already
  shipped keeps its ship if the retrospective is interrupted.
- The loop leaves an Interrupted item alone. It does not release the claim, does
  not try the host-side finish of BEH-713, and posts no "no PR" breadcrumb. It
  backs off one poll interval and selects again. The breaker does not count it.

## Consequences

- An interrupted ticket stays In Progress. If no run resumes it, the stale-claim
  reaper returns it to Todo after the claim TTL, provided no branch was pushed.
  The next run that selects it reuses its worktree.
- A host that keeps killing sessions no longer trips the breaker. The one-poll
  backoff stops it working through the whole queue at full speed. Each interrupted
  ticket is left In Progress until the reaper returns it.
- An interrupted session's findings dropbox is not filed. The next run of that
  Stage clears it.
- The PR loop's fix sessions (`internal/fix`) do not classify the signal yet. Its
  retrospective does, through the shared Stage body.
