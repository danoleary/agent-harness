# ADR-0015: The worktree is a type

- Status: Accepted
- Date: 2026-09-22

## Context

"The worktree for ticket X" is the harness's central noun. Every Stage creates one,
gates it, checkpoints it, rebases it, pushes it and tears it down. It had no type.

`internal/git` was 976 lines exposing **30 exported free functions and exactly one
exported type** (`RebaseResult`), and the noun travelled as three strings re-derived
at every call site:

```go
WorktreePath(herdPath, slug)
BranchName(branchPrefix, slug)
CreateWorktree(herdPath, branchPrefix, slug)
Push(herdPath, branchPrefix, slug)
CommitSubjects(herdPath, branchPrefix, slug)
BranchPushed(herdPath, branchPrefix, slug)
```

ADR-0013 bound that triple once in `hostio.Real`, which stopped the Stages from
threading it — but only moved the problem down one layer. Inside `internal/git` the
three strings were still loose parameters, and the shape they forced was the real
cost.

### Twelve twin pairs, and the tested half was the wrong half

The file's dominant pattern was an exported wrapper over an unexported twin that
took an injected runner:

```go
func Exported(args) T                     { return exported(args, execRun) }
func exported(args, run commandRunner) T  { /* real logic */ }
```

Twelve of them: `CreateWorktree`, `FetchMain`, `RebaseOntoMain`, `IsRebasedOnto`,
`IsDisjointFrom`, `BranchDiffEmpty`, `BranchDocsOnly`, `AbortRebase`, `Push`,
`PushForceWithLease`, `CheckpointCommit`, `EnsureCIRerunCommit`.

Twelve seams is twelve places to inject, and a test could only reach the lowercase
half — so the half the harness actually runs was the untested one. Per-function
coverage said so exactly:

| exported | covered |
|---|---|
| `Push`, `PushForceWithLease` | 0.0% |
| `CreateWorktree`, `RemoveWorktree` | 0.0% |
| `CommitSubjects`, `BranchPushed` | 0.0% |
| `FetchMain`, `AbortRebase` | 0.0% |

Those are the mutating remote operations — the ones that can strand a verified
branch. `withRetry`, `remoteRetryBudget`, `nonTransient` and `authFailureMarkers`
— the whole BEH-403/BEH-475 transient-failure story that decides whether a green,
gate-passed branch ever reaches origin — were reachable **only** through them.

The package's other operations had no seam at all: `HeadSHA`, `CommitSubjects`,
`WorktreeClean`, `BranchExists`, `BranchPushed` and `GatherTddGroundTruth` called
`exec.Command` directly, so the only way to test them was a real temp repo.

## Decision

`internal/git` gets two types and one seam.

```go
type Checkout struct{ path, branchPrefix string; runners }
type Worktree struct{ repo, branch, path string; runners }

func Open(path, branchPrefix string) Checkout
func (c Checkout) Worktree(slug string) Worktree
```

A `Checkout` is the Consumer's primary checkout. It mints one `Worktree` per ticket
slug, and every operation the harness performs on either is a method:

```go
w := git.Open(projectPath, branchPrefix).Worktree(slug)
w.Create(); w.Clean(); w.Rebase(); w.Push(); w.Checkpoint(key, stage); w.GroundTruth()
```

The seam is one unexported value both types carry:

```go
type runners struct {
	run    commandRunner
	output outputRunner
	pipe   pipeRunner
	sleep  func(time.Duration)
	now    func() time.Time
}
```

Every side effect in the package goes through one of those five fields. `Open`
supplies the production set; a test supplies fakes and then drives **the same
exported methods production calls**. Twelve twin pairs become one hook, and the
untestable half stops existing rather than being worked around.

Which of a `Worktree`'s three strings an operation uses is a fact about git, not
about the caller: the branch-ref reads and both pushes run against the shared
`.git` from the main checkout, the working-tree reads and mutations run against the
worktree path. Both are now internal detail.

## Consequences

- `internal/git` 89.5% → **94.9%**, with nothing left at 0.0%. `Push`,
  `PushForceWithLease`, `Create`, `Remove`, `FetchMain`, `CommitSubjects`,
  `BranchPushed` and `AbortRebase` are now exercised through the exported path,
  and with them the retry budget and the auth fail-fast.
- `git.go` keeps the primitives, the retry machinery and the pure classifiers;
  `worktree.go` holds the two types. Navigating to "what does the harness do to a
  worktree" is one file.
- `hostio.Real`'s Repo/Remote halves become one-line forwards over `h.wt(slug)`.
  The adapter still binds the checkout and the prefix once (ADR-0013); it now hands
  down a value instead of three arguments.
- `internal/git` imported `internal/verify` for `Worktree.GroundTruth`'s return type
  — the wrong-way dependency ADR-0016 removed by moving the gather behind the verify
  decision, splitting that method into `CommitsAhead` + `DisjointHistory`. This ADR
  supplied the type that change was written against.
- Tests that use a real temp repo build a `Worktree` over the production seam and so
  keep pinning the git contracts the scripted tests rest on. Both kinds now call the
  same methods.
