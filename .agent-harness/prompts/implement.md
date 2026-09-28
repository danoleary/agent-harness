/tdd Work on {{.Identifier}}: {{.Title}}.

This is the agent harness working on its own source. Read `CONTEXT.md` for the
vocabulary and the ADRs under `docs/adr/` that the ticket touches before you
design anything. If the change reverses or extends a recorded decision, add or
amend an ADR in the same commit.

Run `make check` as its own Bash call before you commit — it is the gate the
host re-runs and the one CI runs. Commit on `{{.BranchPrefix}}/{{.Slug}}`.
