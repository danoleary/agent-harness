# Tutorial: your first ticket to a pull request

In this tutorial you'll set the harness up on one of your projects, give it one
small GitHub issue, and watch it come back as a tested pull request. It takes
about half an hour, most of it waiting for the first sandbox image to build and
the first session to run.

You'll need:

- a project on GitHub that you can push to, with a test command that passes;
- `docker` running, `git` 2.45 or newer, and `gh`;
- a Claude subscription or an Anthropic API key.

The examples use a Go project at `~/src/myproject` with the GitHub repo
`myorg/myproject`. Substitute your own; the harness works the same way for pnpm,
.NET and Python.

## 1. Install the harness

```bash
curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh
agent-harness --version
```

The installer puts one command, `agent-harness`, in `~/.local/bin`. If the
second line says `command not found`, add `~/.local/bin` to your `PATH`.

## 2. Scaffold the project

```bash
cd ~/src/myproject
agent-harness init
```

`init` detects the toolchain (`go.mod` here) and the GitHub repo (your `origin`
remote) and asks you to confirm each. Press Enter to accept. It writes:

```
.agent-harness/
  config.toml        gates, post_create hook, tracker
  Dockerfile         your sandbox image, FROM the harness's base
  prompts/           one prompt body per Stage
  .env               credentials; gitignored
```

## 3. Add your credentials

Open `.agent-harness/.env`. `PROJECT_PATH` is already filled in. Set two more:

1. **A Claude credential.** For a subscription, run `claude setup-token` and set
   `CLAUDE_CODE_OAUTH_TOKEN` to the token it prints. For an API key, delete that
   line and set `ANTHROPIC_API_KEY` instead.
2. **`GH_TOKEN`.** Create a *classic* personal access token with the `repo`
   scope at GitHub → Settings → Developer settings → Personal access tokens →
   Tokens (classic). A fine-grained token won't work.

## 4. Name your skills

Each file in `.agent-harness/prompts/` starts with a placeholder skill such as
`/TODO-implement-skill`. The harness has no skills of its own, so each body
must name a skill your repository carries.

If your repository already has Claude Code skills for implementing, reviewing
and running a retrospective, put their names in place of the placeholders. If it
doesn't, the harness repo's own skills are a starting point: copy
[`.claude/skills/`](../.claude/skills/) from the harness into your project, adapt
them to your project, and name `/tdd`, `/review-worktree` and `/retrospective`.

Delete each `TODO(agent-harness init)` comment once you've dealt with it.

## 5. Check the gates

Open `.agent-harness/config.toml` and find the `[[gates]]` entries. For a Go
project `init` writes something like:

```toml
[[gates]]
name = "test"
command = "go test ./..."
```

Make them the commands your CI runs. The harness opens a PR only when every gate
passes.

## 6. Run `doctor`

```bash
agent-harness doctor
```

`doctor` checks the tools, the `.env` it read, each credential, the config and
prompts, the tracker labels, and the sandbox image. Every failing line has a fix
under it. The first time, expect it to say the labels don't exist yet. Create
them:

```bash
gh label create ready-for-agent --repo myorg/myproject
gh label create in-progress     --repo myorg/myproject
gh label create agent-harness   --repo myorg/myproject
```

Run `doctor` again and repeat until every line passes.

## 7. Write a ticket

Open a small, self-contained issue, the kind you'd give a new teammate on
their first day, and label it ready:

```bash
gh issue create --repo myorg/myproject \
  --title "Reverse() returns its input unchanged for an empty string" \
  --body "Add a test that Reverse(\"\") returns \"\", and make it pass." \
  --label ready-for-agent
```

The ready label is how a person decides what the harness may work unattended.
Nothing without it is ever picked.

## 8. Do a dry run

```bash
agent-harness pipeline --next --dry-run
```

You should see your issue's Key, its title, and the plan: the three Stages and
the gates. Nothing is claimed and no container starts.

## 9. Run it for real

```bash
agent-harness pipeline --next
```

The first run builds your sandbox image, which takes a few minutes. Then it:

1. claims the issue (it gets the `in-progress` label);
2. runs the **implementation** session, which commits on `feat/gh-<number>`;
3. runs a **review** session that has never seen the implementation, re-runs
   your gates on the host, pushes, opens the PR and watches CI;
4. runs a **retrospective** over both transcripts and files any Findings.

In another terminal, `agent-harness watch` shows which Stage is running.

## 10. Look at what it made

```bash
gh pr list --repo myorg/myproject
```

There's your pull request, with the test and the fix. The session transcripts
are in `.agent-harness/logs/#<number>/`. Review the PR as you would a
colleague's, and merge it.

## What you've done

You installed the harness, gave it a project, and took one ticket from a label to
a gate-green PR without touching the code yourself.

From here:

- Let it work the whole queue with `agent-harness start`, and stop it with
  `touch .agent-harness/STOP`. See the [CLI reference](reference/cli.md).
- Use [Linear](how-to/linear.md) or [Jira](how-to/jira.md) instead of GitHub
  Issues.
- Write [better prompt bodies](how-to/prompt-bodies.md) for your conventions.
- When something goes wrong, start at [troubleshooting](troubleshooting.md).
