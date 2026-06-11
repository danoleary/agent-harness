// Package skills holds guard tests pinning the Claude Code skills the harness
// drives to the contract the harness depends on. The harness never reads these
// SKILL.md files at runtime — it injects a prompt that names the skill — but
// the skills' *behaviour* is the harness's contract, so drift in either
// direction (e.g. the /retrospective skill dropping the "always write []"
// instruction the findings parser relies on) must fail a test, not a
// production run.
package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from this test file to the repo root (the dir that holds
// the shared `.claude/skills/` tree). The skill files live at the repo root,
// outside the agent-harness Go module, so the test resolves them by path.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".claude", "skills")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (.claude/skills) above " + thisFile)
		}
		dir = parent
	}
}

// retrospectiveSkill reads the /retrospective SKILL.md or fails the test.
func retrospectiveSkill(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".claude", "skills", "retrospective", "SKILL.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// TestRetrospectiveSkillIsDiscoverable is the tracer: the skill exists and
// carries valid frontmatter (a `name: retrospective` and a description), so
// Claude Code discovers it as `/retrospective` like `/tdd` and
// `/review-worktree` (AC: "discoverable like /tdd and /review-worktree").
func TestRetrospectiveSkillIsDiscoverable(t *testing.T) {
	src := retrospectiveSkill(t)

	if !strings.HasPrefix(src, "---\n") {
		t.Fatal("SKILL.md must open with a `---` frontmatter block")
	}
	end := strings.Index(src[4:], "\n---")
	if end == -1 {
		t.Fatal("SKILL.md frontmatter block is not closed with `---`")
	}
	frontmatter := src[4 : 4+end]

	if !regexp.MustCompile(`(?m)^name:\s*retrospective\s*$`).MatchString(frontmatter) {
		t.Errorf("frontmatter must declare `name: retrospective`; got:\n%s", frontmatter)
	}
	if !regexp.MustCompile(`(?m)^description:\s*\S`).MatchString(frontmatter) {
		t.Errorf("frontmatter must declare a non-empty `description:`; got:\n%s", frontmatter)
	}
}

// TestRetrospectiveSkillDocumentsDropboxContract pins the findings dropbox
// contract the harness's findings parser depends on: the fixed path
// `/findings/out.json`, the `{title, body, kind}` shape, and — critically —
// the "always write the file, even as []" rule that lets an absent file mean
// "the step never ran" rather than "ran, found nothing". If the skill drops
// any of these, the harness's ground-truth check (out.json present) or its
// parser (internal/findings) silently diverges from what the agent produces.
func TestRetrospectiveSkillDocumentsDropboxContract(t *testing.T) {
	src := retrospectiveSkill(t)

	if !strings.Contains(src, "/findings/out.json") {
		t.Error("skill must name the dropbox path `/findings/out.json`")
	}
	// Assert the *quoted* JSON-key form the parser reads (obj["title"] etc.),
	// not the bare word — "kind" appears in the skill's prose ("what kind of
	// work provoked the friction"), so a bare-substring check would pass even
	// if the field doc were dropped.
	for _, field := range []string{"title", "body", "kind"} {
		if !strings.Contains(src, `"`+field+`"`) {
			t.Errorf("skill must document the `%s` finding field as a JSON key", field)
		}
	}
	// The always-write rule: the file must be written even with no findings,
	// as an empty array `[]`.
	if !strings.Contains(src, "[]") {
		t.Error("skill must instruct writing an empty array `[]` when there are no findings")
	}
	lower := strings.ToLower(src)
	if !strings.Contains(lower, "always") {
		t.Error("skill must state the file is *always* written (absent file => step never ran)")
	}
}

// TestRetrospectiveSkillDocumentsInputs pins the input contract: the skill
// studies the *sessions*, so it must read every prior transcript for the
// ticket (implementation + review) plus the diff. The transcripts live at the
// ticket-keyed log path the harness writes (`agent-harness/logs/BEH-NNN/`),
// visible in the container via the real-path mount (ADR-0002).
func TestRetrospectiveSkillDocumentsInputs(t *testing.T) {
	src := retrospectiveSkill(t)
	lower := strings.ToLower(src)

	if !strings.Contains(lower, "transcript") {
		t.Error("skill must instruct reading the session transcript(s)")
	}
	if !strings.Contains(lower, "every") {
		t.Error("skill must instruct reading *every* prior transcript for the ticket, not just one")
	}
	if !strings.Contains(src, "logs/BEH-") {
		t.Error("skill must point at the ticket-keyed transcript path `logs/BEH-NNN/`")
	}
	if !strings.Contains(lower, "diff") {
		t.Error("skill must instruct reading the diff alongside the transcripts")
	}
}

// TestRetrospectiveSkillForbidsRemoteAndCodeChanges pins the guardrails: the
// skill writes only the dropbox. It must not change code, push, or reach Linear
// / any MCP server — the harness owns all remote I/O (ADR-0001/0002) and files
// the findings itself after reading out.json. The sandbox has no GH/Linear
// credential, but the prompt-level guardrail keeps the agent from wasting a
// session trying.
func TestRetrospectiveSkillForbidsRemoteAndCodeChanges(t *testing.T) {
	src := retrospectiveSkill(t)
	lower := strings.ToLower(src)

	if !strings.Contains(lower, "linear") {
		t.Error("skill must explicitly forbid touching Linear")
	}
	if !strings.Contains(lower, "mcp") {
		t.Error("skill must explicitly forbid MCP calls")
	}
	if !strings.Contains(lower, "push") {
		t.Error("skill must explicitly forbid pushing")
	}
	// Forbids editing code / the feature — phrased as "no code changes" or
	// "do not edit"/"read-only".
	if !regexp.MustCompile(`(?is)(no code change|not? (edit|change|modify|touch).{0,40}code|read-only|do not commit)`).MatchString(src) {
		t.Error("skill must state it makes no code changes (read-only on the repo)")
	}
}

// TestRetrospectiveSkillScopesFindingsToHarness pins the scope: findings are
// about the harness/environment (setup friction, systemic gaps, missing
// patterns), explicitly NOT about the feature diff (that was review's job).
// Without this the agent drifts into re-reviewing the code and the dropbox
// fills with feature bugs the harness then mis-files as harness improvements.
func TestRetrospectiveSkillScopesFindingsToHarness(t *testing.T) {
	src := retrospectiveSkill(t)
	lower := strings.ToLower(src)

	if !strings.Contains(lower, "harness") || !strings.Contains(lower, "environment") {
		t.Error("skill must scope findings to the harness/environment")
	}
	// Must explicitly contrast with the feature so the agent doesn't re-review
	// the code.
	if !regexp.MustCompile(`(?is)not.{0,30}feature|feature.{0,30}(not|never)`).MatchString(src) {
		t.Error("skill must state findings are NOT about the feature diff")
	}
	// Must name the systemic-vs-one-off distinction so one-off slips aren't
	// filed as harness improvements.
	if !strings.Contains(lower, "systemic") {
		t.Error("skill must use the systemic-vs-one-off distinction to gate findings")
	}
}
