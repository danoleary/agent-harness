# ADR-0045: A triage loop moves agent-ready tickets into the ready queue

- Status: Accepted
- Date: 2026-11-08
- Builds on: ADR-0019 (a loop is a Source and a Pipeline), ADR-0033 (agent-ready unless it needs a human)

## Context

The ticket loop works only tickets a human has labelled `ready_label` (on Linear,
also in a Todo state). The label is the blast-radius gate: a person decides what
runs unattended. In practice most backlog tickets that are never labelled are
not risky. Nobody has got round to reading them. A ticket filed by a person, or a
Linear ticket left in Backlog, waits until someone triages it by hand. Meanwhile
the ticket loop idles on `queue empty`.

ADR-0033 already lets the harness mark its own findings ready unless they need a
human. Triage is the same judgement, applied to tickets a person filed.

## Decision

**A third built-in loop, `triage`, judges the tickets not yet marked ready and
marks the ones an agent can start.**

1. **Source `backlog`.** It lists the tracker's open, unassigned tickets that
   carry neither the ready label nor the blocked label: on Linear, the team's
   issues in a backlog or unstarted state; on GitHub, open issues that are not PRs
   and do not carry the in-progress label. The tracker port gains a separate
   capability, `tracker.Backlog` (`Untriaged`, `MarkReady`), instead of a fourth
   layer of `Tracker`. Jira's ready queue is a JQL query the harness cannot add a
   ticket to, so its adapter does not have it, and a triage loop over Jira is
   refused when it starts.
2. **Step `assess`.** One read-only Triage session per ticket, with the checkout
   mounted so it can check the ticket against the code. The envelope states a
   default bar that leans toward ready: a ticket is held back only when its goal
   is unclear, it needs a person's decision, a credential or a UI step, it is too
   large or not a task, or it depends on unmerged work or is already done. A
   Consumer body for `assess` leads with the project's own bar. The ground truth
   is the verdict file `/findings/verdict.json`: `{ready, reason, questions}`,
   and a not-ready verdict must list at least one question.
3. **Step `apply`.** A ready ticket gets the ready label, plus a short comment
   with the reason. On Linear, a ticket in a backlog state first moves to the
   team's unstarted "Todo" state. It moves first and is labelled second, so a
   failed label leaves an unlabelled Todo ticket that triage lists again. The
   other order would leave a labelled ticket in Backlog, where neither selection
   nor triage looks. A ticket that is not ready gets one comment with the
   questions. A loop without `apply` records verdicts and changes no ticket, so a
   Consumer can try triage out first.
4. **One verdict per ticket text.** A verdict is recorded against a hash of the
   ticket's title and description in `.agent-harness/locks/triage/<slug>.json`,
   and only after `apply` acted on it. A ticket is not judged again until a
   human edits it. Labels and comments are left out of the hash, so the
   harness's own comment does not invalidate its own verdict.
5. **No lease.** Triage never claims a ticket: it does not move it to In
   Progress, so there is nothing to reap or release. A recorded verdict, ready or
   not, is `Shipped` for the breaker. A run that reaches no verdict, or cannot act
   on it, is `NoPR`, so three in a row trip the breaker as for any loop.

## Alternatives considered

- **Heuristics instead of a session** (a description longer than N, has an
  acceptance-criteria heading). This is cheap but wrong both ways. A terse ticket
  over obvious code is ready, and a long one with an open design question is
  not. The judgement is the point. Rejected.
- **Record verdicts in hidden marker comments on the ticket**, as the PR loop
  does with attempts. That would keep the state with the ticket and across
  machines, but it needs a comment-list read in every adapter. Triage runs as one
  daemon per checkout, like the PR loop, so local state is enough. Deferred.
- **Assign the ticket to the bot instead of labelling it.** The ready label is
  already the queue's contract on both trackers. Rejected.

## Consequences

- The ready label is no longer applied only by a person. It is applied by a
  person or by the triage loop when it runs. A Consumer that wants the human gate
  back runs no triage loop, or runs it without `apply`.
- Each untriaged ticket costs one session the first time and one more after each
  edit. `max_items` and `poll_interval` bound the spend.
- `retro` does not run on a `backlog` loop: a triage run files no findings.
