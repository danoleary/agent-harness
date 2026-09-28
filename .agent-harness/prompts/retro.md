/retrospective for {{.Identifier}}. The worktree is at `.claude/worktrees/{{.Slug}}` on branch `{{.BranchPrefix}}/{{.Slug}}`.

Read **every** prior transcript for this ticket under `.agent-harness/logs/{{.Identifier}}/` (the `implementation-*.jsonl` and `review-*.jsonl` streams) plus the feature branch's diff against `main`. Study the sessions, not the feature — review already judged the code.

This repo IS the harness, so the two audiences are close: use `harness` for friction in the sandbox, the prompt envelope, or the stage flow as you experienced it running; use `project` for friction in this repo's own tests, gates, scripts or docs.
