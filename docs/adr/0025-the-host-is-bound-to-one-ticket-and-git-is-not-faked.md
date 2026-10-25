# ADR-0025: The Host is bound to one ticket, and git is not faked

- Status: Accepted
- Date: 2026-10-25
- Amends: ADR-0013 (the Repo half of "the Fake is the second adapter"), ADR-0015

## Context

ADR-0013 gave the Stages one host port with two adapters, `hostio.Real` and
`hostio.Fake`. ADR-0015 then gave the worktree a type, `git.Worktree`, and noted
the consequence: `hostio.Real`'s Repo half became one-line forwards over it.

```go
func (h *Real) X(slug string) T { return h.wt(slug).X() }
```

By #28 that was 22 of the Repo interface's methods, and the forwarding was the
whole of them. It had three costs.

- **Adding one git read touched four places**: `git.Worktree`, `hostio.Repo`,
  `Real` and `Fake`.
- **The Fake modelled git as unrelated booleans** — `Exists`, `Clean`,
  `DiffEmpty`, `Rebased`, `Disjoint`, `Ahead`, `CollapseOnRebase`,
  `HeadOnRebase`. They could state impossible combinations (no worktree, yet
  clean; ahead by one straight after `CreateWorktree`), and they could not change
  mid-run without a per-test subclass: `worktreeLost`, `regrafting`, and
  `h.Clean = false` set inside an `AgentFn`. The Stage bugs this harness has
  shipped live in exactly those changes: a rebase that collapses a branch, a
  regraft that heals one, a session that leaves edits uncommitted.
- **The slug was threaded through about 60 call sites**, though every Host only
  ever serves one ticket. The composition roots already build one per slice.

## Decision

**A Host is bound to one ticket and hands out that ticket's `git.Worktree` whole.
Its git half is never scripted: `hostio.Fake` runs over a real repository.**

```go
type Repo interface {
	Worktree() gitpkg.Worktree
	TicketAlreadyOnMain(key string) bool
	ResolvedAdvisory(key, description string) string
}
```

- `hostio.New`, `hostio.NewOnBranch` and `hostio.NewPreview` take the slug. A
  Stage calls `h.Worktree()` and acts on the value: `wt.Clean()`,
  `wt.Checkpoint(key, stage)`, `wt.RegraftOntoBase(git.BaseRef)`. The 22
  forwards, and the re-exported rebase constants, are deleted.
- **Remote stays a seam.** `FetchMain`, `Push`, `PushForceWithLease`, `CreatePR`,
  `PRExists`, `OpenPRExists`, `ChecksReadable` and `WatchCI` take no slug and
  stay on the Host. Pushes are the worktree's own methods, but a Stage pushes
  through the Host so a test can fail one. Unscripted, the Fake's fetch and push
  really run against the checkout's origin.
- **`verify.GroundTruth` takes `git.Worktree`'s method set** (`Exists`, `Clean`,
  `CommitsAhead`, `DisjointHistory`, `DiffEmpty`, `BranchExists`,
  `BranchPushed`, `IsRebasedOnto`). A Stage hands verify its worktree directly.
  `WorktreeReap` takes the one non-git read, the PR lookup, as a func, so the
  push is still checked before gh is asked.
- **`ship.Port`** is `Worktree()` plus the remote and tracker calls. `Replay`
  rebases, aborts and checks for a disjoint history on the worktree itself.
- **`internal/gittest`** builds the repositories. Each has a checkout, a local
  bare origin, and a second clone that stands in for everyone else who pushes to
  main. It builds the states the Stages act on: a handoff commit, uncommitted
  edits, a sibling's merge (conflicting, or landing the same fix), a disjoint
  root, an uncreatable worktree, a resolved conflict. A session that commits does
  so with git from inside `AgentFn`. Like `net/http/httptest` it is in the
  non-test build so every package's tests can import it.

## Alternatives considered

- **A stateful in-memory worktree model**, where commits, the clean flag and the
  base move together. It would keep Stage tests fast, but it is a git simulator
  the harness would have to keep faithful by hand, and every divergence from git
  is a bug the tests cannot see. Git is local, so the real thing is available.
  Rejected.
- **Keep the slug-keyed Repo and back only the Fake with a temp repo.** This
  removes the impossible states but keeps the 22 forwards and the four-place
  edit. Rejected: the forwarding was half the problem.
- **Fault-inject pushes with a pre-receive hook on the bare origin.** That is
  more faithful still, but a rejected push goes through `withRetry`'s real
  backoff budget unless the error is classed permanent. A scripted error on the
  Host is cheaper and asserts the same Stage behaviour. Rejected for now.

## Consequences

- A git operation is added in `internal/git` and nowhere else.
- Real git has already corrected one test. The old `CollapseOnRebase` flag let
  the committed-fix recovery reach a post-rebase collapse that it cannot reach:
  after its own fetch, a branch whose change is already on main reads as an empty
  diff, and the recovery declines before it replays. The BEH-680 race it guards
  is a sibling landing the same fix *and* main moving on past it. The tests now
  build that.
- Stage tests are slower. `internal/stages`, `internal/ship` and `internal/prloop`
  went from about 0.06s to about 10s combined, since each test builds a repository
  and runs real git. `go test ./...` runs the packages in parallel, so the gate
  pays less than the sum. `t.Parallel()` inside those packages is the next lever
  if it grows.
- `internal/stages` imports `internal/git` again, for `git.BaseRef`. ADR-0013
  removed that import because the Stages called git's free functions. Now they
  hold a `git.Worktree` that the Host hands them, so the import names the value
  rather than reaching around the port.
- Assertions that read the Fake's call log for git operations (`rebase proj-1`,
  `checkpoint PROJ-9 tdd`, `abort-rebase`) now read git: the branch is on
  origin/main, HEAD's subject is the checkpoint message, the tip did not move. The
  call log still records the sandbox and the remote.
