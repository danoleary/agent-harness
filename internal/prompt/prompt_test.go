package prompt

import (
	"regexp"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/ticket"
)

var sample = ticket.Ticket{
	Identifier:  "BEH-362",
	Title:       "Agent harness Phase 1: run-tdd CLI",
	Description: "## What to build\n\nA minimal CLI.\n\n## Acceptance criteria\n\n- [ ] fetch the ticket",
	Priority:    "Urgent",
}

func TestBuildTddInvokesSkillOnTicketAndSlug(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	for _, want := range []string{"/tdd", "BEH-362", "slug `beh-362`"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// The whole pipeline (verify, push, PR, CI, review, rebase) keys off the
// canonical `feat/<slug>` branch. If the prompt leaves the branch prefix to the
// agent's judgement, a bug-fix ticket invites `fix/<slug>`, whose committed,
// gate-passing handoff the harness then never sees on the empty `feat/<slug>` it
// checks — the work is silently stranded (BEH-615). So the prompt must pin the
// prefix: instruct the canonical `new-worktree.sh <slug> feat` verbatim, the same
// way every other prompt hardcodes `feat/<slug>`.
func TestBuildTddPinsFeatBranchPrefix(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !strings.Contains(p, "new-worktree.sh beh-362 feat") {
		t.Errorf("prompt does not pin the canonical `new-worktree.sh <slug> feat` command:\n%s", p)
	}
}

func TestBuildTddInjectsTitleAndDescription(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	for _, want := range []string{sample.Title, "Acceptance criteria", "fetch the ticket"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// BEH-619: an umbrella/batch ticket defers its real work to sub-issues, but the
// sandbox is isolated from Linear (ADR-0002) — so mid-session the agent can't
// fetch a child's spec and under-delivers (it reached for an unavailable
// mcp__linear-server__get_issue and shipped 1 of ~9 children). The host fetches
// each child host-side; BuildTdd must inline every child's id + title + body,
// clearly delimited, and tell the agent to implement them all.
func TestBuildTddInlinesSubIssueSpecs(t *testing.T) {
	umbrella := ticket.Ticket{
		Identifier:  "BEH-520",
		Title:       "Lint/boundary guard sweep",
		Description: "Batch the small static-rule tickets.",
		SubIssues: []ticket.SubIssue{
			{Identifier: "BEH-293", Title: "no-forced-open-modal rule", Description: "Forbid `open={true}` on a controlled modal."},
			{Identifier: "BEH-381", Title: "story-module boundary", Description: "Stories must import via the module seam."},
		},
	}
	p := BuildTdd(umbrella, "beh-520")

	for _, want := range []string{
		"BEH-293", "no-forced-open-modal rule", "Forbid `open={true}` on a controlled modal.",
		"BEH-381", "story-module boundary", "Stories must import via the module seam.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing inlined sub-issue content %q", want)
		}
	}
	// Must steer the agent to implement every child, not just the umbrella body.
	if !regexp.MustCompile(`(?i)(every|all|each).{0,40}sub-?issue`).MatchString(p) {
		t.Error("prompt does not tell the agent to implement every sub-issue")
	}
	// Must say the children are already fetched / not to look them up (the sandbox
	// can't reach Linear).
	if !regexp.MustCompile(`(?i)(already.*fetch|do not.*look.*up|cannot reach Linear)`).MatchString(p) {
		t.Error("prompt does not say the sub-issues are pre-fetched / unreachable from the sandbox")
	}
}

// An ordinary ticket with no sub-issues must read exactly as before — no empty
// umbrella header, no dangling "sub-issue" steer.
func TestBuildTddOmitsSubIssueSectionWhenNone(t *testing.T) {
	p := BuildTdd(sample, "beh-362")
	if regexp.MustCompile(`(?i)sub-?issue`).MatchString(p) {
		t.Error("prompt with no sub-issues should not carry a sub-issue section")
	}
}

func TestBuildTddSteersOffLinear(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !regexp.MustCompile(`(?i)already.*(claimed|In Progress)`).MatchString(p) {
		t.Error("prompt does not say the ticket is already claimed/In Progress")
	}
	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("prompt does not steer off Linear")
	}
}

// The sandboxed Claude CLI intermittently mangles bash calls that chain a pipe
// into head/tail with a command substitution like `cd "$(...)"`, producing
// opaque errors (`head: invalid number of bytes`, `cd: too many arguments`) that
// look like the agent's bug rather than an environment quirk and cost real turns
// (BEH-401). The harness can't patch the upstream CLI, so it steers every
// sandboxed session away from the trigger pattern via the prompt.
func assertCarriesBashQuirkSteer(t *testing.T, p, label string) {
	t.Helper()
	if !regexp.MustCompile(`(?i)head`).MatchString(p) || !strings.Contains(p, `$(`) {
		t.Errorf("%s does not name the bash-quirk trigger (pipe into head + `$(...)`)", label)
	}
	if !regexp.MustCompile(`(?i)one .{0,20}per (Bash )?call|single .{0,20}call`).MatchString(p) {
		t.Errorf("%s does not give the one-command-per-call workaround", label)
	}
	// BEH-598: the same opaque-error surface ALSO hits a plain command that exits
	// non-zero BY DESIGN — `git merge-base` on disjoint histories exits 1 and the
	// tool collapses it to a bare `Error` with the exit code + stderr stripped, so
	// the agent can't tell an expected non-zero exit from a real break. The steer
	// must name that second case and give the inspect-the-exit-code workaround.
	if !regexp.MustCompile(`(?i)merge-base`).MatchString(p) {
		t.Errorf("%s does not name the plain non-zero-exit case (git merge-base)", label)
	}
	if !regexp.MustCompile(`(?i)non-?zero`).MatchString(p) {
		t.Errorf("%s does not mention a non-zero exit collapsing to a bare Error", label)
	}
	if !regexp.MustCompile(`(?i)exit code|echo exit|\$\?`).MatchString(p) {
		t.Errorf("%s does not give the inspect-the-exit-code workaround", label)
	}
	// BEH-601: a THIRD face of the same quirk — a `VAR=value; cmd "$VAR"`
	// assignment-then-use within ONE Bash call can expand $VAR to the empty
	// string, so the failure is silent (empty output) or surfaces as a path with
	// the prefix missing (e.g. `/dist`). `&&`-chaining the assignment to its use
	// expands fine; `;`-separating it is what drops the variable. The steer must
	// name the variable-assignment case and give a workaround.
	if !regexp.MustCompile(`(?i)assign`).MatchString(p) {
		t.Errorf("%s does not name the intra-call variable-assignment case", label)
	}
	if !regexp.MustCompile(`(?i)empty`).MatchString(p) {
		t.Errorf("%s does not say the assignment can expand to the empty string", label)
	}
	if !strings.Contains(p, "&&") {
		t.Errorf("%s does not give the &&-chain workaround for the assignment case", label)
	}
	// BEH-645: a FOURTH face — and the worst, because it makes a GREEN gate look
	// RED. Chaining trailing statements onto a GATE command in one Bash call (e.g.
	// `pnpm run check > log 2>&1; echo exit=$?; grep … | head`) can concatenate
	// those statements as ARGUMENTS onto the gate's own command (the `oxfmt
	// --check` inside `pnpm run check` receives them as file args), so the gate
	// fails with `Expected at least one target file` and pnpm emits `[ELIFECYCLE]`
	// — a spurious failure on a gate that actually PASSED. The steer must name that
	// symptom and give the run-each-gate-as-its-own-call workaround.
	if !regexp.MustCompile(`(?i)ELIFECYCLE|Expected at least one target file`).MatchString(p) {
		t.Errorf("%s does not name the spurious gate false-red symptom (ELIFECYCLE / Expected at least one target file)", label)
	}
	if !regexp.MustCompile(`(?i)gate`).MatchString(p) {
		t.Errorf("%s does not name the gate-command false-red case", label)
	}
	if !regexp.MustCompile(`(?i)(its )?own .{0,20}call`).MatchString(p) {
		t.Errorf("%s does not give the run-each-gate-as-its-own-call workaround", label)
	}
}

// BEH-544: a ticket can be dispatched as live work after its fix already merged
// (often under a *sibling* ticket the host-side own-key guard can't catch). The
// prompt must steer the agent to verify the ticket's cited symbols/premise still
// hold before planning, and — if a grep shows the work already landed — to NOT
// fabricate a no-op change but record "already resolved, recommend close".
func TestBuildTddSteersToVerifyPremiseBeforePlanning(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !regexp.MustCompile(`(?i)(verify|confirm|check).*(still|already)`).MatchString(p) {
		t.Error("prompt does not steer the agent to verify the premise still holds")
	}
	if !regexp.MustCompile(`(?i)already (resolved|fixed|landed|merged)`).MatchString(p) {
		t.Error("prompt does not mention the already-resolved outcome")
	}
	if !regexp.MustCompile(`(?i)(recommend|suggest) clos`).MatchString(p) {
		t.Error("prompt does not tell the agent to recommend close when the work has landed")
	}
	if !regexp.MustCompile(`(?i)no-?op|do not (fabricate|invent|manufacture)`).MatchString(p) {
		t.Error("prompt does not warn against fabricating a no-op change")
	}
}

func TestBuildTddCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildTdd(sample, "beh-362"), "tdd prompt")
}

// BEH-554: when the dispatched ticket's OWN feat branch already carries un-merged
// fix commits (a *resumed* worktree from a prior session), the host swaps in this
// prompt. It must steer the agent to inspect the branch's existing commits before
// planning and prefer verify-and-handoff over re-implementing a fix that may
// already be complete — keyed on the branch's own history, not main.
func TestBuildTddResumedBranchSteersToVerifyExistingCommits(t *testing.T) {
	p := BuildTddResumedBranch(sample, "beh-362")

	for _, want := range []string{"/tdd", "BEH-362", "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("resumed-branch prompt missing %q", want)
		}
	}
	// Must say the branch already carries commits for this ticket.
	if !regexp.MustCompile(`(?i)already.{0,40}(commit|fix)`).MatchString(p) {
		t.Error("resumed-branch prompt does not say the branch already has commits/a fix")
	}
	// Must steer to inspect that history (git log) before planning.
	if !regexp.MustCompile(`(?i)git log`).MatchString(p) {
		t.Error("resumed-branch prompt does not tell the agent to read the branch's git log")
	}
	// Must steer toward verify-and-handoff, away from re-implementing.
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,40}(re-?implement|rewrite|redo)`).MatchString(p) {
		t.Error("resumed-branch prompt does not warn against re-implementing already-done work")
	}
	if !regexp.MustCompile(`(?i)already (resolved|fixed|done|complete)`).MatchString(p) {
		t.Error("resumed-branch prompt does not mention the already-fixed outcome")
	}
}

// The resumed-branch prompt must keep every cross-cutting steer the standard tdd
// prompt carries — off Linear, findings to the dropbox, and the bash-quirk
// workaround — so swapping it in never silently drops a guard.
func TestBuildTddResumedBranchKeepsStandardSteers(t *testing.T) {
	p := BuildTddResumedBranch(sample, "beh-362")

	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("resumed-branch prompt does not steer off Linear")
	}
	if !strings.Contains(p, "/findings/out.json") {
		t.Error("resumed-branch prompt missing the findings dropbox path")
	}
	assertCarriesBashQuirkSteer(t, p, "tdd resumed-branch prompt")
}

// BEH-619: the resumed-branch and resume prompts are direct swap-ins for the
// implementation BuildTdd prompt, so an umbrella ticket reaching either path must
// still get its child sub-issue specs inlined — otherwise the children silently
// vanish on a resume/retry and the umbrella under-delivers exactly as before.
func TestBuildTddResumedBranchInlinesSubIssues(t *testing.T) {
	umbrella := ticket.Ticket{
		Identifier: "BEH-520",
		Title:      "Lint/boundary guard sweep",
		SubIssues:  []ticket.SubIssue{{Identifier: "BEH-381", Title: "story-module boundary", Description: "Import via the seam."}},
	}
	p := BuildTddResumedBranch(umbrella, "beh-520")
	for _, want := range []string{"BEH-381", "story-module boundary", "Import via the seam."} {
		if !strings.Contains(p, want) {
			t.Errorf("resumed-branch prompt missing inlined sub-issue content %q", want)
		}
	}
}

func TestBuildTddResumeInlinesSubIssues(t *testing.T) {
	umbrella := ticket.Ticket{
		Identifier: "BEH-520",
		Title:      "Lint/boundary guard sweep",
		SubIssues:  []ticket.SubIssue{{Identifier: "BEH-381", Title: "story-module boundary", Description: "Import via the seam."}},
	}
	p := BuildTddResume(umbrella, "beh-520", sampleWorktree)
	for _, want := range []string{"BEH-381", "story-module boundary", "Import via the seam."} {
		if !strings.Contains(p, want) {
			t.Errorf("resume prompt missing inlined sub-issue content %q", want)
		}
	}
}

func TestBuildTddRedirectsFindingsToDropbox(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !strings.Contains(p, "/findings/out.json") {
		t.Error("prompt missing the findings dropbox path")
	}
	if !regexp.MustCompile(`title.*body.*kind.*key`).MatchString(p) {
		t.Error("prompt missing the {title, body, kind, key} finding shape")
	}
}

// On a retry after a usage-policy refusal (BEH-389), the worktree and branch
// already exist with the surviving diff. The resume prompt must steer the agent
// to continue in that existing worktree and commit the work — never to recreate
// it (which would fail on the already-existing branch) — while keeping every
// other steer (Linear off, findings to the dropbox).
func TestBuildTddResumeSteersToExistingWorktree(t *testing.T) {
	p := BuildTddResume(sample, "beh-362", sampleWorktree)

	for _, want := range []string{"/tdd", "BEH-362", sampleWorktree, "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("resume prompt missing %q", want)
		}
	}
	// Must tell the agent the worktree already exists and to resume, NOT recreate.
	if !regexp.MustCompile(`(?i)already exist`).MatchString(p) {
		t.Error("resume prompt does not say the worktree already exists")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,40}(create|recreate|new-worktree)`).MatchString(p) {
		t.Error("resume prompt does not forbid recreating the worktree")
	}
	if !regexp.MustCompile(`(?i)commit`).MatchString(p) {
		t.Error("resume prompt does not tell the agent to commit the surviving work")
	}
}

func TestBuildTddResumeCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildTddResume(sample, "beh-362", sampleWorktree), "tdd resume prompt")
}

func TestBuildTddResumeStillSteersOffLinearAndToDropbox(t *testing.T) {
	p := BuildTddResume(sample, "beh-362", sampleWorktree)

	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("resume prompt does not steer off Linear")
	}
	if !strings.Contains(p, "/findings/out.json") {
		t.Error("resume prompt missing the findings dropbox path")
	}
}

func TestBuildRetrospectiveInvokesSkillOnTicket(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", nil)

	for _, want := range []string{"/retrospective", "BEH-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// The retrospective studies the *sessions*, so the prompt must point it at the
// ticket-keyed transcripts and the diff.
func TestBuildRetrospectivePointsAtTranscriptsAndDiff(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", nil)

	if !strings.Contains(p, "logs/BEH-362") {
		t.Error("prompt does not point at the ticket-keyed transcript path logs/BEH-362/")
	}
	if !regexp.MustCompile(`(?i)every.*transcript`).MatchString(p) {
		t.Error("prompt does not say to read every transcript")
	}
	if !regexp.MustCompile(`(?i)diff`).MatchString(p) {
		t.Error("prompt does not mention the diff")
	}
}

// The dropbox contract: the fixed path, the finding shape, and the always-write
// rule that makes an absent file mean "the step never ran".
func TestBuildRetrospectiveCarriesDropboxContract(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", nil)

	if !strings.Contains(p, "/findings/out.json") {
		t.Error("prompt missing the findings dropbox path")
	}
	if !regexp.MustCompile(`title.*body.*kind.*key`).MatchString(p) {
		t.Error("prompt missing the {title, body, kind, key} finding shape")
	}
	if !strings.Contains(p, "[]") || !regexp.MustCompile(`(?i)always`).MatchString(p) {
		t.Error("prompt must instruct always writing the file, even as []")
	}
	// The key drives cross-run dedup, so the prompt must explain its purpose.
	if !regexp.MustCompile(`(?i)dedup`).MatchString(p) {
		t.Error("prompt must explain the key's dedup purpose so re-runs converge to one issue")
	}
}

// On a re-run, the prompt must list the already-filed finding classes and tell
// the session to treat them as settled and look only for NEW friction — so it
// doesn't burn its budget re-deriving issues a prior run already filed (BEH-539).
func TestBuildRetrospectiveListsAlreadyFiledFindingsOnRerun(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", []FiledFinding{
		{Key: "sandbox-build-oom", Title: "Build OOM-killed at prerender"},
		{Key: "sandbox-storybook-oom", Title: "Storybook test runner OOMs"},
	})

	for _, want := range []string{"sandbox-build-oom", "sandbox-storybook-oom", "Build OOM-killed at prerender"} {
		if !strings.Contains(p, want) {
			t.Errorf("re-run prompt missing already-filed entry %q", want)
		}
	}
	if !regexp.MustCompile(`(?i)(settled|already.*filed|do not re-)`).MatchString(p) {
		t.Error("re-run prompt does not tell the session to treat already-filed findings as settled")
	}
	if !regexp.MustCompile(`(?i)new`).MatchString(p) {
		t.Error("re-run prompt does not steer the session toward NEW friction only")
	}
}

// A title-only finding (no explicit key) must still appear in the settled list.
func TestBuildRetrospectiveListsTitleOnlyAlreadyFiledFinding(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", []FiledFinding{
		{Title: "vitest hung in watch mode"},
	})
	if !strings.Contains(p, "vitest hung in watch mode") {
		t.Error("re-run prompt missing the title-only already-filed entry")
	}
}

// First run (no already-filed findings): the prompt carries no settled-context
// section, so it reads exactly as it did before BEH-539.
func TestBuildRetrospectiveOmitsSettledSectionOnFirstRun(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", nil)
	if regexp.MustCompile(`(?i)already.*filed|treat.*as settled`).MatchString(p) {
		t.Error("first-run prompt should not carry an already-filed/settled section")
	}
}

func TestBuildRetrospectiveCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildRetrospective(sample, "beh-362", nil), "retrospective prompt")
}

func TestBuildRetrospectiveForbidsRemoteAndCodeChanges(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362", nil)

	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("prompt does not steer off Linear")
	}
	if !regexp.MustCompile(`(?i)(no|not?|don't).{0,20}(code|push|commit)`).MatchString(p) {
		t.Error("prompt does not forbid code changes/push/commit")
	}
}

const sampleWorktree = "/Users/dan/herd/.claude/worktrees/beh-362"

func TestBuildReviewInvokesSkillOnWorktreePath(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	for _, want := range []string{"/review-worktree", sampleWorktree, "BEH-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildReviewInjectsTicketContextForIntent(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	// Review reconstructs intent from the ticket; it needs the title + ACs.
	for _, want := range []string{sample.Title, "Acceptance criteria", "fetch the ticket"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildReviewIsCold(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	// Cold review: reconstruct from branch/issue/diff, never the implementation transcript.
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,40}transcript`).MatchString(p) {
		t.Error("prompt does not steer the review off the implementation transcript (coldness is the point)")
	}
}

func TestBuildReviewCommitsLocallyOnly(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	if !regexp.MustCompile(`(?i)commit.{0,30}local`).MatchString(p) {
		t.Error("prompt does not tell review to commit fixes locally")
	}
	// The harness owns the push + PR (ADR-0002) — the agent must not.
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,20}push`).MatchString(p) {
		t.Error("prompt does not forbid pushing")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,20}(run )?gh\b|(do not|don't|never).{0,30}(open|raise).{0,20}PR`).MatchString(p) {
		t.Error("prompt does not forbid running gh / opening the PR")
	}
}

func TestBuildReviewCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildReview(sample, "beh-362", sampleWorktree), "review prompt")
}

// BEH-525: the review session runs in a memory-constrained sandbox where the heavy
// gates OOM-kill it. Because the prompt strips the skill's push/PR/findings steps,
// the agent might think emitting the report is pointless and skip it — so the prompt
// must reinforce: do the lenses FIRST and ALWAYS emit the "## Review:" report (the
// harness keys off it to tell a real review from one cut short by an OOM mid-gate).
func TestBuildReviewSteersLensesFirstAndEmitsVerdict(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	if !regexp.MustCompile(`(?i)(lens|review).{0,60}(before|first).{0,60}(gate|build|lint|storybook)`).MatchString(p) {
		t.Error("prompt does not steer the lenses to run before the memory-heavy gates")
	}
	if !strings.Contains(p, "## Review:") {
		t.Error("prompt does not tell the agent to emit the \"## Review:\" verdict report (the harness's completeness signal)")
	}
}

// BEH-580: the autonomous pipeline has no human to answer the skill's approval
// prompt, so the prompt must steer the reviewer to self-resolve findings and end the
// verdict in a machine-read `Disposition:` line — `blocked` for a finding it can't
// resolve (the harness fails the push closed) rather than asking a question that
// never gets answered and shipping the finding unaddressed.
func TestBuildReviewSteersDispositionAndSelfResolve(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	if !strings.Contains(p, "Disposition:") {
		t.Error("prompt does not tell the agent to end the verdict with a machine-read Disposition: line (the harness's push decision)")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,40}(ask|wait|approval)`).MatchString(p) {
		t.Error("prompt does not steer the reviewer off asking/waiting for approval (no human in the autonomous pipeline)")
	}
	if !regexp.MustCompile(`(?i)blocked`).MatchString(p) {
		t.Error("prompt does not mention the blocked disposition for an unresolvable finding")
	}
}

func TestBuildReviewForbidsLinearAndFindings(t *testing.T) {
	p := BuildReview(sample, "beh-362", sampleWorktree)

	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("prompt does not steer off Linear")
	}
	// Findings are retrospective's job now, not review's.
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,30}finding`).MatchString(p) {
		t.Error("prompt does not steer review off emitting findings (retrospective owns that)")
	}
}
