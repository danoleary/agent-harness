# ADR-0018: The dry-run plan is the Stages' own run specs

- Status: Accepted
- Date: 2026-09-30
- Extends: ADR-0013

## Context

ADR-0013 put every container behind a `Runner` that mints the `--name`, so "a
caller never names a container" became structural — for launches. `pipeline --dry-run`
was left outside it. `pipeline.Plan` was a second, hand-copied description of
every Stage's containers:

- it called `sandbox.BuildDockerRunArgs` / `BuildWorktreeCommandArgs` itself,
  re-copying the `sandbox.Config` fields three times;
- it minted container names itself (`"%s<run-id>-<pid>-%s"`), the one thing
  ADR-0013 says a caller never does;
- it re-derived the logs root and the findings dropbox paths, and re-listed every
  label (`prep`, `gate-<name>`, `postcreate`) and every model.

Each Stage also kept its own `--dry-run` printer, a third description. The plan's
tests asserted strings, so nothing noticed when a Stage changed what it launched
and the plan did not.

## Decision

**Each Stage describes its runs once, as the `hostio.AgentRun` / `hostio.ShellRun`
specs it launches, and the dry-run plan is those specs rendered through the Runner.**

- One builder per run (`implementationRun`, `postCreateRun`, `prepRun`,
  `reviewRun`, `gateRun`, `retrospectiveRun` in `internal/stages/plan.go`). The
  Stage bodies launch what they build, adding only the launch-time `Retry`
  schedule (how a run is retried, not what it runs) and the relabel of an in-stage
  re-launch.
- `stages.ImplementationPlan`, `ReviewPlan` and `RetrospectivePlan` compose those
  builders into a `StagePlan`, in launch order, with notes for the host-side
  worktree creation and a deliberately skipped prep. `StagePlan.Render` previews
  each run through a `hostio.Planner` — the one renderer every `--dry-run` uses.
- `hostio.NewPreview(cfg)` is the Runner with no run: the same naming and argv
  building, stamped `<run-id>` / `<pid>` placeholders. It is a `Planner`, not a
  `Host`, so nothing can launch through it.
- `pipeline.Plan` concatenates the three Stages' plans under the pipeline's own
  ordering and skip semantics. It holds no knowledge of any container.

The test that keeps it honest runs all three Stages over `hostio.Fake` and asserts
the plans equal the `AgentRun` / `ShellRun` specs the Fake recorded, field for
field (`stages.TestPlanIsWhatTheStagesLaunch`).

## Consequences

- Container naming is back behind the Runner seam for the plan too.
- Three dry-run printers are gone; a standalone Stage's `--dry-run` and the
  pipeline plan render the same `StagePlan`.
- `runlog.TicketDir` and `runlog.FindingsDir` are the pure path rules the plan and
  a live `runlog.Logger` share, so a plan can name the dropbox without creating it.
- The standalone Stages' `--dry-run` output gains the host-side steps the pipeline
  plan already showed (worktree creation, a skipped prep), and every section is
  titled by its run (`--- review docker command ---`).
- Runs a Stage launches only on a recovery path — the CI auto-fix session, the
  pre-push conflict session, a refusal or review re-launch — are variants of a
  planned spec or are not planned at all, exactly as before.
