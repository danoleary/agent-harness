# Write the prompt bodies

Each Stage's prompt is the harness's **contract envelope** wrapped around your
**Project body** (ADR-0009). The envelope carries the findings-dropbox protocol,
the tracker-off steer, the branch and handoff contract, the sanitize rule and the
injected ticket context. You can't remove it. Your body says which skill to
invoke and what your project's conventions are.

## 1. Create the three files

You need all three, in `.agent-harness/prompts/`:

| File | Stage | What the skill it names should do |
|---|---|---|
| `implement.md` | Implementation | implement the ticket and commit on the branch |
| `review.md` | Review | review the worktree cold and commit any fixes locally |
| `retro.md` | Retrospective | study the session transcripts for friction |

`agent-harness init` writes all three with placeholder skills
(`/TODO-implement-skill`, …). A missing or blank body stops the run at config
load, and the error names every file that is missing.

## 2. Name a skill your repository carries

The harness declares no skill of its own, because it can't know which skills your
project has. Replace each placeholder with a skill that exists in **your**
repository, for example `.claude/skills/tdd/SKILL.md` invoked as `/tdd`. If the
skill isn't there, the session has nothing to run.

## 3. Fill in the template fields

A body is a Go `text/template`. These fields interpolate; a body that fails to
parse is used verbatim, so check the dry run if a field doesn't appear:

| Field | Value |
|---|---|
| `{{.Identifier}}` | the ticket Key: `#57`, `PROJ-123` |
| `{{.Title}}` | the ticket title |
| `{{.Slug}}` | the branch slug: `gh-57`, `proj-123` |
| `{{.BranchPrefix}}` | `branch_prefix` from `config.toml` |
| `{{.WorktreePath}}` | the worktree's path, `.claude/worktrees/<slug>` under the checkout |

A minimal implementation body:

```markdown
/tdd Work on {{.Identifier}}.

Run `dotnet test` before you commit. Commit on `{{.BranchPrefix}}/{{.Slug}}`.
```

A minimal review body:

```markdown
/review-worktree {{.WorktreePath}}

You are reviewing the worktree on branch `{{.BranchPrefix}}/{{.Slug}}` for
{{.Identifier}}. Commit any fixes locally. End with a `Disposition:` line.
```

## 4. State your conventions, not the harness's

Put in the body what a new contributor to your project would need to hear: which
gate to run before committing, which docs to update, which directories are
generated. Leave out push, PR and tracker instructions: the envelope already
tells the session the host owns those, and a body that contradicts it only
confuses the session.

A review body does not need the session to run your heavy test suites after it
commits a fix. The host re-runs every `[[gates]]` entry over the same commit, and
when a gate goes red over a commit the review made, the harness re-launches the
review once with the gate's name and the tail of its log. Tell the session to run
the cheap gates and a test scoped to the files it edited.

## 5. Check it

```bash
agent-harness doctor --offline
agent-harness pipeline --next --dry-run
```

`doctor` reports a missing or blank body. The dry run prints the plan for the
next ticket without starting a container.

For a worked example, see [`example/.agent-harness/prompts/`](../../example/.agent-harness/prompts/).
A body for a `[[loops]]` Step, such as `fix`, follows the same rules; see
[declare a loop](declare-a-loop.md).
