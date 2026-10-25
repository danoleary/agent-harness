/TODO-review-skill {{.WorktreePath}}

<!-- TODO(agent-harness init): replace /TODO-review-skill with the skill this
repository carries for a cold review of a feature worktree, then delete this
comment. The harness declares no skill of its own: the one named here must exist
in this repo. -->

You are reviewing the worktree on branch `{{.BranchPrefix}}/{{.Slug}}` for {{.Identifier}}.

This is a COLD review. Reconstruct the intent from the branch, this ticket, and the diff ONLY. Do NOT read the implementation session's transcript — an independent reviewer must not be anchored to the implementer's framing.

Read the diff and apply the review lenses FIRST, before running any memory-heavy gate: the harness re-runs every gate host-side anyway. Whatever else happens, ALWAYS emit your `## Review:` report once the lenses are done.

Commit any fixes LOCALLY. Do NOT push and do NOT open the PR — the harness owns all remote I/O.

This runs UNATTENDED. Self-resolve every finding, then end with a `Disposition:` line: `Disposition: clear — <reason>`, or `Disposition: blocked — <reason>` ONLY for a finding that genuinely needs human judgement.
