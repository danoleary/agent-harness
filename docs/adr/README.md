# Architecture decision records

One file per decision. This index lists every ADR in ascending order, and
`make check` fails if it doesn't (ADR-0034). To add one, take the number after
the last line below (check `origin/main`, not local main), write
`NNNN-<slug>.md`, and append its line at the end. Two branches that take the
same number then conflict here, before either reaches main.

- [ADR-0001](0001-harness-owns-linear-integration.md): The harness owns all Linear I/O; nothing Linear enters the sandbox
- [ADR-0002](0002-harness-owns-remote-io.md): The harness owns all remote I/O; the sandbox is air-gapped except for Anthropic; the checkout is mounted at its real host path
- [ADR-0003](0003-claim-on-select.md): `--next` claims the ticket during selection, before Docker preflight
- [ADR-0004](0004-loop-is-a-long-running-daemon.md): The autonomous loop is a long-running daemon, not drain-and-exit + cron
- [ADR-0005](0005-loop-viewer-is-a-separate-reader-over-a-structured-stream.md): The loop viewer is a separate read-only reader over a structured event stream
- [ADR-0006](0006-loop-viewer-stopped-signal-is-liveness-authoritative.md): The loop viewer's "stopped" signal is liveness-authoritative and loud
- [ADR-0007](0007-extract-harness-to-standalone-open-source-repo.md): Extract the harness into a standalone, open-source repo; herd becomes a Consumer
- [ADR-0008](0008-project-config-lives-in-the-consumer-repo.md): Project configuration and the adapter surface live in the Consumer repo
- [ADR-0009](0009-prompts-are-a-contract-envelope-around-a-project-body.md): Stage prompts are a non-overridable contract envelope around a Consumer-supplied body
- [ADR-0010](0010-tracker-is-a-port-with-linear-jira-github-adapters.md): The issue tracker is a port with Linear, Jira, and GitHub Issues adapters
- [ADR-0011](0011-harness-findings-file-upstream-to-the-public-harness-repo.md): Harness findings are classified at source and filed upstream as issues on the public harness repo
- [ADR-0012](0012-one-command-install-behind-a-single-dispatcher.md): One-command install, behind a single `agent-harness` entrypoint
- [ADR-0013](0013-stages-talk-to-the-host-through-one-port.md): Stages talk to the host through one port
- [ADR-0014](0014-the-daemon-takes-a-host-limits-and-a-clock.md): The daemon takes a Host, Limits and a Clock
- [ADR-0015](0015-the-worktree-is-a-type.md): The worktree is a type
- [ADR-0016](0016-verify-gathers-its-own-ground-truth.md): verify gathers its own ground truth
- [ADR-0017](0017-ship-lands-the-branch.md): ship lands the branch
- [ADR-0018](0018-the-dry-run-plan-is-the-stages-own-run-specs.md): The dry-run plan is the Stages' own run specs
- [ADR-0019](0019-a-loop-is-a-source-and-a-pipeline.md): A loop is a Source and a Pipeline
- [ADR-0020](0020-doctor-checks-the-whole-setup-in-one-pass.md): `agent-harness doctor` checks the whole setup in one pass
- [ADR-0021](0021-init-scaffolds-a-consumer-from-recorded-toolchain-defaults.md): `agent-harness init` scaffolds a Consumer from recorded toolchain defaults
- [ADR-0022](0022-a-branch-that-edits-the-sandbox-dockerfile-is-gated-in-its-own-image.md): A branch that edits the sandbox Dockerfile is gated in its own image
- [ADR-0023](0023-a-ticket-waits-on-an-open-pr-its-body-names.md): A ticket waits on an open PR its body names
- [ADR-0024](0024-several-instances-of-the-ticket-loop-share-a-claim-lock-and-gate-slots.md): Several instances of the ticket loop share a claim lock and gate slots
- [ADR-0025](0025-the-host-is-bound-to-one-ticket-and-git-is-not-faked.md): The Host is bound to one ticket, and git is not faked
- [ADR-0026](0026-one-stage-frame-and-one-pipeline-composition-root.md): One Stage frame, and one pipeline composition root
- [ADR-0027](0027-loop-reclaims-disk-by-pruning-merged-worktrees.md): The autonomous loop reclaims disk by pruning merged worktrees
- [ADR-0028](0028-a-ready-ticket-with-open-questions-is-dispatched-with-a-warning.md): A ready ticket with open questions is dispatched with a warning
- [ADR-0029](0029-docs-are-split-by-diataxis-and-live-in-the-repo.md): The docs are split by Diátaxis and live in the repo, not a wiki
- [ADR-0030](0030-a-signal-killed-session-is-interrupted-not-failed.md): A signal-killed session is Interrupted, not failed
- [ADR-0031](0031-the-ticket-loop-can-leave-ci-to-the-pr-loop.md): The ticket loop can leave CI to the PR loop
- [ADR-0032](0032-a-ticket-that-names-files-in-flight-waits-for-them.md): A ticket that names files in flight waits for them
- [ADR-0033](0033-a-filed-finding-is-agent-ready-unless-it-needs-a-human.md): A filed finding is agent-ready unless it needs a human
- [ADR-0034](0034-adr-numbers-are-reserved-in-an-append-only-index.md): ADR numbers are reserved in an append-only index
- [ADR-0035](0035-a-conflict-resolutions-edits-beyond-the-conflict-are-reviewed-before-push.md): A conflict resolution's edits beyond the conflict are reviewed before push
