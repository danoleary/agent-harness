---
name: review-worktree
description: Cold-review a feature worktree in this Go repo against its ticket, fix any real defect in a local commit, and report a Disposition. Use when the harness review stage invokes /review-worktree <path>.
---

# /review-worktree — cold review of a finished branch

The argument is the worktree path. The prompt around this skill gives the ticket
and the branch.

## 1. Reconstruct the intent

From the ticket and `git log <base>..HEAD` / `git diff <base>...HEAD` only,
where `<base>` is the commit the prompt names as the branch's base (else
`origin/main`). Never diff against local `main`: the harness does not update it,
so it may be stale and pull in other tickets' commits. Do not read the
implementation transcript. Write one line: what should now be true.

## 2. Read the diff through these lenses, in order

1. **Spec** — does the diff do what the ticket asked, all of it, and nothing
   unrelated? Is any acceptance criterion missing?
2. **Correctness** — error paths, nil/empty inputs, context cancellation,
   goroutine and file-handle leaks, off-by-ones, exit codes (see DESIGN.md
   §Exit-code contract).
3. **Invariants** — the sandbox never reaches a remote and never sees
   GH_TOKEN or a tracker credential (ADR-0002); the prompt envelope stays
   non-overridable (ADR-0009); harness findings are sanitized (ADR-0011).
4. **Tests** — does a test fail if the change is reverted? Are they testing
   behaviour through a public interface rather than internals?
5. **Design** — does it deepen a module or add a shallow pass-through? Does it
   use `CONTEXT.md` vocabulary? Is a changed decision recorded in an ADR?
6. **Docs** — README, `docs/CONSUMER.md`, `docs/DESIGN.md`, the example config.

Do the reading before running any gate.

## 3. Fix

Fix every real finding yourself. Then run `make check` as its own Bash call and
commit the fixes locally as one commit: `Review fixes for <ticket> over <short
HEAD you reviewed>`, so stacked review commits from different runs can be told
apart. Do not change unrelated code, and do not rewrite the implementer's commit.

No real finding means no commit. A cosmetic tidy-up is not a finding.

If the prompt says the branch is UNCHANGED SINCE THE LAST REVIEW and
`git rev-parse HEAD` matches the HEAD it names, the diff already has a verdict:
skip the lenses, run `make check`, and re-emit the prior Disposition line in your
report. Commit only for a real defect, and name it.

## 4. Report

```
## Review: <ticket>

Intent: <one line>

Findings:
- [fixed|accepted] <lens>: <what, where, and why it matters>

Gate: make check <green|red>

Disposition: clear — <reason>
```

Use `Disposition: blocked — <reason>` only when a finding needs a human decision
(an ambiguous spec, a trade-off an ADR does not settle). Never block on
something you could fix.
