# ADR-0071: A handoff that left work uncommitted reaches review as a named checkpoint

- Status: Accepted
- Date: 2026-11-24
- Extends: ADR-0016 (verify gathers its own ground truth), ADR-0062 (a re-run on a branch that stands on its verified handoff starts at review)

## Context

`verify.Tdd` passes an implementation session on its commits: a worktree, and
at least one commit ahead of `origin/main`. It never read the working tree. In
one run (#431) the session committed its first slice, then hit its wall-clock
cap (exit 137) in its final gates with the last slice green but uncommitted.
The check reported "one commit ahead", the recovery checkpoint (BEH-479) did
not run because it runs only on a failed verdict, and review started with the
normal prompt over a dirty tree. The review session found the slice by chance
in `git status`, reviewed it and wrote its handoff. A less careful review could
have cleared only the committed range, and the host Gate re-run judges the
working files, so it could test a tree that differs from the committed one.

Any cut-off between a slice's green and its commit leaves this state. The
per-slice commit steer narrows the window but cannot close it.

## Decision

**After an implementation session whose handoff passes, a dirty worktree is
committed as a harness recovery checkpoint, and a review over a branch whose
tip is a checkpoint is told so.**

1. **The implementation stage** checks the tree after `verify.Tdd` passes, and
   after the handoff strip. When it is dirty it commits it with
   `Worktree.Checkpoint` (the `checkpoint(harness): ` subject, which ADR-0062
   already reads as harness-made), unless another live run holds the worktree
   (#366). It logs `⚠ the implementation session <how it ended> with work
   uncommitted in the worktree (<paths>) …` to `run.jsonl`, naming the exit.
   This holds whatever the exit code: an exit-0 session that left work behind
   handed off no more than a killed one. The verdict stays OK, so review runs.
2. **No verified handoff is recorded** for such a session (ADR-0062 point 1): a
   re-run starts at implementation, not at a review of work no session finished.
3. **The review stage** reads the run of checkpoints at the branch tip from git
   (`Worktree.TipCheckpoint`): the newest, the commit they sit on, and the paths
   they change. A checkpoint a later commit sits on was handed off by that
   commit's session and is not named. The state lives in git rather than in a
   record, so a re-run, or a review checkpoint (BEH-559) a killed review left,
   reads the same way.
4. **The review envelope** then carries a section naming the checkpoint, its
   range and its paths. It tells the session to review that work as
   unhanded-off, check that a test covers each change, and amend the checkpoint
   into a handoff commit with the headings the project's handoff conventions ask
   of an implementation commit. The harness does not name those headings: a
   `Seams:` record is one Consumer's convention, not a harness one.

## Alternatives considered

- **Pass a flag from implementation to review instead of committing.** The host
  Gate would still run over a tree that differs from the committed one, and the
  flag would have to survive a re-run in a record. A commit is the ground truth
  every later step already reads.
- **Re-dispatch a short implementation resume when the dirty diff is large.**
  Left out: it needs a size threshold nothing yet justifies, and review already
  re-runs every Gate over what it commits.
- **Fail `verify.Tdd` on a dirty tree.** That would throw the committed slice
  back to a whole new implementation session over a branch that already holds
  most of the work.
