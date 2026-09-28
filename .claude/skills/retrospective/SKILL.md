---
name: retrospective
description: Study a ticket's implementation and review transcripts for friction in the process and file it as findings. Use when the harness retrospective stage invokes /retrospective.
---

# /retrospective — learn from how the ticket went

You are read-only. You judge the *sessions*, not the feature: review already
judged the code.

## 1. Read

- Every transcript the prompt points at, in order. They are JSON lines; use
  `jq` to pull out tool calls, tool errors and the assistant's reasoning rather
  than reading raw.
- `git diff main...HEAD` in the worktree, only to understand what the sessions
  were trying to do.

## 2. Look for friction

- **Wasted turns** — retries of the same failing command, a tool quirk worked
  around by trial and error, a gate that went red for an environmental reason.
- **Missing context** — something the agent had to rediscover that a prompt
  body, skill, `CONTEXT.md` or doc should have told it.
- **Contradictions** — the envelope, the prompt body and this repo's skills or
  docs disagreeing with each other.
- **Sandbox gaps** — a missing tool, permission, cache miss or memory limit.
- **Gate problems** — slow, flaky, or reporting the wrong thing.

Ignore one-off mistakes that a better instruction would not have prevented.

## 3. Write findings

Each finding is one fixable problem with a concrete suggested change. Write it
so that it could be picked up as a ticket on its own:

- **title** — the problem, imperatively phrased as its fix.
- **body** — what happened (the failure class, not transcript excerpts), why it
  cost time, and the specific change that prevents it: which file, which rule.
- **kind** — e.g. `prompt`, `skill`, `sandbox`, `gate`, `docs`.
- **key** — a stable slug for the failure class, so re-runs dedupe.
- **audience** — as the envelope defines it.

Three sharp findings beat ten vague ones. Zero is a fine answer: write `[]`.
