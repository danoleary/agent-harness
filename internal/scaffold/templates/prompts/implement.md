/TODO-implement-skill Work on {{.Identifier}}.

<!-- TODO(agent-harness init): replace /TODO-implement-skill with the skill this
repository carries for implementing a ticket test-first (for example
.claude/skills/tdd/), then delete this comment. The harness declares no skill of
its own: the one named here must exist in this repo. -->

Run << range $i, $g := .Gates >><< if $i >> and << end >>`<< $g.Command >>`<< end >> before you commit — the harness
re-runs every gate host-side before it pushes. Commit on
`{{.BranchPrefix}}/{{.Slug}}`.
