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

func TestBuildTddInjectsTitleAndDescription(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	for _, want := range []string{sample.Title, "Acceptance criteria", "fetch the ticket"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
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
}

func TestBuildTddCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildTdd(sample, "beh-362"), "tdd prompt")
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
	p := BuildRetrospective(sample, "beh-362")

	for _, want := range []string{"/retrospective", "BEH-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// The retrospective studies the *sessions*, so the prompt must point it at the
// ticket-keyed transcripts and the diff.
func TestBuildRetrospectivePointsAtTranscriptsAndDiff(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362")

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
	p := BuildRetrospective(sample, "beh-362")

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

func TestBuildRetrospectiveCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildRetrospective(sample, "beh-362"), "retrospective prompt")
}

func TestBuildRetrospectiveForbidsRemoteAndCodeChanges(t *testing.T) {
	p := BuildRetrospective(sample, "beh-362")

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
