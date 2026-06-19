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

func TestBuildTddRedirectsFindingsToDropbox(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !strings.Contains(p, "/findings/out.json") {
		t.Error("prompt missing the findings dropbox path")
	}
	if !regexp.MustCompile(`title.*body.*kind`).MatchString(p) {
		t.Error("prompt missing the {title, body, kind} finding shape")
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
	if !regexp.MustCompile(`title.*body.*kind`).MatchString(p) {
		t.Error("prompt missing the {title, body, kind} finding shape")
	}
	if !strings.Contains(p, "[]") || !regexp.MustCompile(`(?i)always`).MatchString(p) {
		t.Error("prompt must instruct always writing the file, even as []")
	}
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
