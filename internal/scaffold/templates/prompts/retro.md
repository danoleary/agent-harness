/TODO-retro-skill for {{.Identifier}}. The worktree is at `.claude/worktrees/{{.Slug}}` on branch `{{.BranchPrefix}}/{{.Slug}}`.

<!-- TODO(agent-harness init): replace /TODO-retro-skill with the skill this
repository carries for a session retrospective, then delete this comment. The
harness declares no skill of its own: the one named here must exist in this repo. -->

Read **every** prior transcript for this ticket under `.agent-harness/logs/{{.Identifier}}/` (the `implementation-*.jsonl` and `review-*.jsonl` streams) plus the feature branch's diff against `main`. Study the sessions, not the feature — friction in the process or the environment, never the feature code (that was review's job).
