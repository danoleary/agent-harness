# ADR-0025: One Stage frame, and one pipeline composition root

- Status: Accepted
- Date: 2026-10-25
- Extends: ADR-0013

## Context

ADR-0013 gave the three Stages one port, `hostio.Host`. It left them each
writing the same opening and closing steps against it:

- the `KindStageStart` record, with its `(dry-run)` suffix;
- `h.Tracker()` and then `FetchTicket`, with a "fetched …" line;
- `MkdirAll` on the findings dir, the disk-full warning (BEH-540) and
  `ClearDropbox`;
- the `KindSandboxLaunch` record before each session and the "session exited
  (code N) — transcript at logs/…" line after it.

Only the disk-full warning text had been shared. The copies had already drifted
apart. The implementation stage named the ticket's priority and the other two
did not. The review stage's launch record said "launching review session" where
the other two said "launching sandbox".

ADR-0013 also named the `cmd/` entrypoints as the composition root. That is two
roots for one pipeline. `cmd/pipeline` and `loophost.TicketPipeline` each built
the same `pipeline.Deps` literal over a `hostio.New`. `cmd/loop`, `cmd/pipeline`
and `hostio.Real` each mapped the config's credentials onto `trackers.Secrets`
by hand.

## Decision

**The Stages share one frame, the ticket pipeline has one composition root, and
the tracker has one constructor from config.**

- `stages.frame` holds the shared steps: `start` (the stage-start record),
  `fetch` (tracker, ticket, narration), `dropbox` (mkdir, disk-full, clear) and
  `launch` (the sandbox-launch record, the session, the transcript line). It is
  tested once, in `frame_test.go`.
- The frame is a set of steps, not a wrapper that takes the body as a function.
  The bodies put their own policy between the steps. The retrospective's skip
  comes before the fetch. The implementation's already-on-main skip and its
  claim come between the dropbox and the launch. A wrapper could own the rest
  only by moving one of those.
- The Docker preflight and the findings filing stay in the bodies. Each Stage
  handles a failed preflight in its own way: only implementation releases the
  claim and reports `PreflightAborted`. The implementation dropbox is filed
  without classification. The retrospective's is routed by audience (ADR-0011).
- `pipeline.ForTicket(cfg, log, runID, args)` builds the slice's one Host and
  binds the three Stages to it. `cmd/pipeline` and the ticket loop both call it.
  The loop then drops the retrospective when its Steps leave out `retro`.
- `trackers.FromConfig(cfg)` is `trackers.New` with the config's credentials.
  The upstream sink still calls `New`, because it is a GitHub sink addressed by
  the `[feedback]` repo, not the Consumer's tracker.

## Consequences

- The run log changes in three places, all toward one wording. Every Stage's
  "fetched" line names the priority. Every session's exit line names its Stage
  ("review session exited …", "implementation session exited …"). Every launch
  record reads "launching sandbox (…cap N min active)", and the review's launch
  record keeps its attempt count as a note.
- The retrospective reads the already-filed findings before it creates the
  findings dir, rather than just after it. A missing dir reads as no prior
  dropbox, so the session gets the same list.
- The ticket loop and `cmd/pipeline` cannot bind a slice differently. A change to
  how a slice is wired is made once, in `ForTicket`.
- The PR loop (`internal/prloop`) keeps its own stage-start record. Its item is a
  PR, not a fetched ticket. It reaches the frame through `RetrospectiveOver`.
