/review-worktree {{.WorktreePath}}

You are reviewing the worktree on branch `{{.BranchPrefix}}/{{.Slug}}` for {{.Identifier}}.

This is a COLD review. Reconstruct the intent from the branch, this ticket, and the diff ONLY. Do NOT read the implementation session's transcript — an independent reviewer must not be anchored to the implementer's framing.

Commit any fixes LOCALLY. Do NOT push and do NOT run `gh` or open the PR — the harness owns all remote I/O (ADR-0002).

This runs UNATTENDED — there is no human to approve anything. Self-resolve every finding, then emit your `## Review:` report and end with a `Disposition:` line: `Disposition: clear — <reason>` when everything is fixed or accepted-and-documented, or `Disposition: blocked — <reason>` ONLY for a finding that genuinely needs human judgement.
