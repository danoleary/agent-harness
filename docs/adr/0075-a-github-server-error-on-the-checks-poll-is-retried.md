# ADR-0075: A GitHub server error on the checks poll is retried like a network error

- Status: Accepted
- Date: 2026-11-26
- Extends: ADR-0074 (A checks poll GitHub refused is a failed run, not a pass)

## Context

The CI watch polls a PR's checks with `gh pr checks`. Since #194 it retries a
call that never reached GitHub: a dial or read timeout, a reset, DNS, gh's
offline message, a 502/503/504 from GitHub's edge, or a gh call that hung. Up
to five in a row, one poll interval apart, are retried before the watch gives
up and fails soft to "CI watch unavailable". Any other gh error ended the watch
at once as a poll fault ("could not poll CI"). #194 put an HTTP 500 on that
side on purpose: GitHub had answered, so the call was not a network blip.

In #471 a pr-doctor run's fix session committed, both local gates passed, the
fix was pushed, and the re-poll got GitHub GraphQL's one-off "Something went
wrong while executing your query" (exit 1). The watch ended as "could not poll
CI (not a red CI)". The pushed fix was never watched to its result, and about
fifteen minutes of session and gates were lost to a server fault that the next
call would not have hit.

## Decision

1. **A GitHub server fault is transient.** A `gh pr checks` failure whose
   stderr carries an HTTP 500, GraphQL's "Something went wrong while executing
   your query", or GraphQL's "couldn't respond to your request in time" is
   `errPollTransient`, the same as a network failure. GitHub answered, but not
   about CI, and the fault is GitHub's, not the PR's. It gets the #194 retry:
   up to five in a row, one poll interval apart, and any answer resets the
   count.
2. **An answer about the request still stops the watch at once.** A 401 or a
   403 keeps its own retry-once path (ADR-0074). A PR or branch gh cannot find,
   a 404 or a 422 is a poll fault on the first call: more polling does not
   change it.
3. **There is no bare "timeout" signature.** gh echoes the branch name in its
   not-found error, so a branch named for a timeout would read as transient and
   be retried five times before failing soft. The timeouts matched are the
   transport's own wording and GraphQL's.
4. **A run of server faults fails soft, like a run of network errors.** The
   reason reads "CI watch unavailable: network or server errors reaching
   GitHub, …" with the last error.

## Consequences

- A one-off GitHub server error no longer ends a PR or security-alerts run. The
  pushed fix is watched to its result.
- A GitHub outage that answers every poll with a 500 for five intervals now
  fails soft to a pass, as an unreachable GitHub already did, rather than
  ending as a poll fault.
