/retrospective for {{.Identifier}}. The worktree is at `.claude/worktrees/{{.Slug}}` on branch `{{.BranchPrefix}}/{{.Slug}}`.

Read **every** prior transcript for this ticket under `.agent-harness/logs/{{.Identifier}}/` (the `implementation-*.jsonl` and `review-*.jsonl` streams) plus the feature branch's diff against `main`. Study the sessions, not the feature — friction in the harness or the environment, never the feature code (that was review's job).

This is an EXAMPLE Consumer body (ADR-0009).
