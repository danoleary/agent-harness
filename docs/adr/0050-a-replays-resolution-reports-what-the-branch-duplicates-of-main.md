# ADR-0050: A replay's resolution reports what the branch duplicates of main

- Status: Accepted
- Date: 2026-11-14
- Extends: ADR-0035

## Context

When the pre-push replay onto origin/main hits a content conflict, a sandboxed
session resolves it, the gates are re-run, and ADR-0035's scoped review looks at
what the session wrote beyond the conflicted files. All of that judges whether
the resolution is sound. None of it asks whether the branch still makes sense on
the new base.

In #255 a ticket merged to main while a branch was in flight. It added a
hand-off-to-a-human capability, a new port method and a Disposition, in the same
area where the branch had built its own hand-off path. The session kept both
sides wherever the text conflicted and took everything else as it merged. The
replayed branch had two parallel ways of doing the same thing. The first review
judged the branch against the old base, so it never saw the overlap. The re-gate
only proves the result compiles and passes its tests, so the PR went out as
reviewed and clear.

## Decision

**The replay's resolution session looks for what main gained underneath the
branch and reports what it leaves duplicated. The PR body carries the report.**

- **The prompt has a step for it.** After the conflicts are resolved, and before
  the `_premerge` marker is dropped, the session lists what origin/main added
  since the old base in the files the branch touches (`git diff _premerge...origin/main`):
  new interface methods, Dispositions, helpers. Where the branch reimplements one
  and switching to main's version is small and mechanical, it switches. A switch
  is an edit like any other, so ADR-0035's scoped review covers it when it falls
  outside the conflicted files.
- **The rest is a structured report.** The session writes the overlaps it did not
  switch to `/findings/overlaps.json`, a JSON array of
  `{"file", "main_added", "branch_duplicates"}`. No file, a blank one or `[]`
  means no overlap. The host mounts the dropbox only for this session
  (`logs/<key>/findings/rebasefix`) and clears a stale report before launch.
- **The host reads it.** `fix.Env.ResolveConflict` reads the report after the
  session, narrates it, and returns it as `ConflictResult.Overlaps`. A report it
  cannot parse is narrated and carries nothing.
- **The PR says so, and still ships.** The review stage adds an `## Overlap with
  `main`` section to the PR body, one line per overlap. A reported overlap does
  not re-run the review and does not hold the push.

## Alternatives considered

- **Re-run the review against the new base when an overlap is reported.** It
  would catch more, but it costs a full review session on a branch that already
  passed one. The overlap is a design question for the human reading the PR, not
  a defect a second automated review is better placed to settle.
- **Have the host compute the overlap itself.** git can list what main added, but
  not whether the branch's code does the same thing. That judgement needs the
  session.
- **Put the report in the session's final message.** The host would have to parse
  free text out of a transcript. A file under `/findings/` is the contract the
  triage verdict already uses (ADR-0045).

## Consequences

- Only the replay (`fix.Rebase`) gets the step. The merge variant, for a branch a
  human opted in to the PR loop, is unchanged and has no dropbox.
- The PR loop's replay and the CI watch's reactive rebase use the same prompt, but
  their PR is already open, so the report is only narrated in `run.jsonl`. They
  can edit the PR body as #267 does if that turns out not to be enough.
- The report is the session's say-so. Nothing checks that it found every overlap,
  so it narrows the gap rather than closing it.
