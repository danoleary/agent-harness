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

// reviewWorktreeFile reads a named file from the /review-worktree skill dir or
// fails the test. The review skill is split across SKILL.md (workflow + lens
// table) and DIMENSIONS.md (per-lens checklists), so callers name the part.
func reviewWorktreeFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".claude", "skills", "review-worktree", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// frontmatter extracts the YAML frontmatter block (between the opening `---\n`
// and the next `\n---`) from a SKILL.md body, or fails the test. Several skill
// contracts assert against the frontmatter (the line Claude Code surfaces when
// choosing a skill), so the extraction lives here once.
func frontmatter(t *testing.T, src string) string {
	t.Helper()
	if !strings.HasPrefix(src, "---\n") {
		t.Fatal("SKILL.md must open with a `---` frontmatter block")
	}
	end := strings.Index(src[4:], "\n---")
	if end == -1 {
		t.Fatal("SKILL.md frontmatter block is not closed with `---`")
	}
	return src[4 : 4+end]
}

// TestReviewWorktreeSkillDocumentsSimplificationLens pins BEH-454: the review
// skill must carry a Simplification lens in DIMENSIONS.md so the reviewer
// actively flags incidental complexity (duplication, dead code, over-built
// abstractions, bespoke code that an existing helper/primitive replaces) rather
// than silently passing it. Without a pinned checklist the lens is one prose
// edit away from being dropped, and the harness's only signal that review
// covers simplification is this skill's text.
func TestReviewWorktreeSkillDocumentsSimplificationLens(t *testing.T) {
	src := reviewWorktreeFile(t, "DIMENSIONS.md")

	if !regexp.MustCompile(`(?im)^##\s+Simplif`).MatchString(src) {
		t.Error("DIMENSIONS.md must document a `## Simplification` lens section")
	}
}

// TestReviewWorktreeSkillListsSimplificationInLensTable pins that the
// Simplification lens is wired into the workflow, not just the appendix: the
// step-3 lens table in SKILL.md must carry a Simplification row. A lens the
// reviewer never iterates (a DIMENSIONS section with no table entry) is dead
// text — the table is the loop the review session actually walks.
func TestReviewWorktreeSkillListsSimplificationInLensTable(t *testing.T) {
	src := reviewWorktreeFile(t, "SKILL.md")

	// A markdown table row naming the lens: `| **Simplification** | ... |`.
	if !regexp.MustCompile(`(?im)^\|\s*\*\*Simplif\w*\*\*\s*\|`).MatchString(src) {
		t.Error("SKILL.md step-3 lens table must include a **Simplification** row")
	}
}

// TestReviewWorktreeFrontmatterNamesSimplificationLens pins that the
// discoverable frontmatter description — the one line Claude Code surfaces when
// choosing a skill — advertises the simplification lens and its now-seven-lens
// count. The description previously hard-coded "six lenses (correctness, ...)";
// if the count and parenthetical aren't updated alongside the table, the skill
// under-sells its own coverage and the stale "six" silently contradicts the
// workflow.
func TestReviewWorktreeFrontmatterNamesSimplificationLens(t *testing.T) {
	fm := frontmatter(t, reviewWorktreeFile(t, "SKILL.md"))

	if !regexp.MustCompile(`(?i)simplif`).MatchString(fm) {
		t.Error("frontmatter description must name the simplification lens")
	}
	if regexp.MustCompile(`(?i)six lenses`).MatchString(fm) {
		t.Error("frontmatter still says `six lenses`; the simplification lens makes it seven")
	}
}

// TestRetrospectiveSkillIsDiscoverable is the tracer: the skill exists and
// carries valid frontmatter (a `name: retrospective` and a description), so
// Claude Code discovers it as `/retrospective` like `/tdd` and
// `/review-worktree` (AC: "discoverable like /tdd and /review-worktree").
func TestRetrospectiveSkillIsDiscoverable(t *testing.T) {
	fm := frontmatter(t, retrospectiveSkill(t))

	if !regexp.MustCompile(`(?m)^name:\s*retrospective\s*$`).MatchString(fm) {
		t.Errorf("frontmatter must declare `name: retrospective`; got:\n%s", fm)
	}
	if !regexp.MustCompile(`(?m)^description:\s*\S`).MatchString(fm) {
		t.Errorf("frontmatter must declare a non-empty `description:`; got:\n%s", fm)
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

// TestRetrospectiveSkillWritesDropboxIncrementally pins BEH-536: the skill must
// write the findings dropbox *incrementally* — as soon as it has findings, then
// append/rewrite as it goes — and must NOT defer the only write to the very end.
// The retrospective is a long, read-heavy step prone to the same environmental
// OOM/cap-kill as the rest of the pipeline; a session that finished its analysis
// but was killed before a single deferred write loses everything and the
// absent-file contract mislabels it as "never ran" (the BEH-536 incident). The
// skill text is the only place this ordering lives, so drift back to
// "write it last" must fail a test.
func TestRetrospectiveSkillWritesDropboxIncrementally(t *testing.T) {
	src := retrospectiveSkill(t)
	lower := strings.ToLower(src)

	// Must instruct incremental writing (write-then-append/update), not a single
	// final write.
	if !regexp.MustCompile(`(?i)incremental|append|as you (go|find)|keep (it )?updat`).MatchString(src) {
		t.Error("skill must instruct writing the dropbox incrementally (write early, then append/update) — not deferring the only write to the end")
	}
	// Must name the motivation: a deferred/late write loses work to an OOM/kill.
	// `\bkill` (not bare `kill`) so the word "skill" — which appears throughout —
	// does not satisfy the check; it must be a real kill/killed/OOM mention.
	if !regexp.MustCompile(`(?i)\boom\b|\bkill`).MatchString(src) {
		t.Error("skill must explain WHY (a late OOM/kill loses a deferred write), so the ordering isn't silently reverted")
	}
	// Must not still carry the old "write it last" instruction that this fix
	// reverses (the exact prose the BEH-536 incident traced to).
	if strings.Contains(lower, "write it last") {
		t.Error("skill must no longer instruct writing the dropbox LAST — that is the BEH-536 regression being fixed")
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
