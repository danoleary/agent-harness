/review-worktree {{.WorktreePath}}

You are reviewing the worktree on branch `{{.BranchPrefix}}/{{.Slug}}` for {{.Identifier}}.

This is a COLD review. Reconstruct the intent from the branch, this ticket, and the diff ONLY. Do NOT read the implementation session's transcript — coldness is the point; an independent reviewer must not be anchored to the implementer's framing.

Do the diff-reading and the review lenses FIRST, before running any memory-heavy gate (build, lint, browser tests). The sandbox is memory-constrained and those gates can OOM-kill the session; the harness re-runs every gate host-side anyway, so running them in-session mostly risks aborting the review before the lenses are applied. Whatever else happens, ALWAYS emit your `## Review:` report once the lenses are done — the harness keys off that header to confirm the qualitative review actually ran.

Commit any fixes LOCALLY. Do NOT push and do NOT run `gh` or open the PR — the harness owns all remote I/O (ADR-0002).

This runs UNATTENDED — there is no human to approve anything. Do NOT ask for approval. Self-resolve every finding, then end with a `Disposition:` line the harness reads as the push decision: `Disposition: clear — <reason>` when everything is fixed or accepted-and-documented, or `Disposition: blocked — <reason>` ONLY for a finding that genuinely needs human judgement.

This is an EXAMPLE Consumer body (ADR-0009). A real one adds that project's own review standards on top.
