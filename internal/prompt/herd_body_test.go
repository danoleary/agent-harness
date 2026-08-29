// Tests for the content of HERD's committed prompt bodies, not the harness's
// envelope.
//
// Each one asserts prose that lives in herd's `.agent-harness/prompts/` — its
// accessible-name steer, its premise check, its context-budget advice. They are
// the Consumer's tests: they read herd's checkout two directories above this
// module, and asserting one project's prose would fail for every other Consumer.
// The extraction (ADR-0007) deletes this file wholesale and herd re-expresses
// these checks as its own guard, so nothing here may be depended on by a test
// that stays in prompt_test.go.
package prompt

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// herdBody reads one of herd's committed Consumer prompt bodies.
func herdBody(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".agent-harness", "prompts", name+".md"))
	if err != nil {
		t.Fatalf("read herd %s body: %v", name, err)
	}
	return string(raw)
}

func hImpl(t *testing.T, tk ticket.Ticket, slug string) string {
	return BuildTdd(tk, slug, testPrefix, herdBody(t, "implement"))
}

func hResumed(t *testing.T, tk ticket.Ticket, slug string) string {
	return BuildTddResumedBranch(tk, slug, testPrefix, herdBody(t, "implement"))
}

func hResume(t *testing.T, tk ticket.Ticket, slug, worktree string) string {
	return BuildTddResume(tk, slug, worktree, testPrefix, herdBody(t, "implement"))
}

func hReview(t *testing.T, tk ticket.Ticket, slug, worktree string) string {
	return BuildReview(tk, slug, worktree, testPrefix, herdBody(t, "review"))
}

// assertCarriesA11yNameSteer checks a prompt carries the BEH-672 accessible-name
// steer. The steer must (a) name the live-region roles whose name is NOT derived
// from descendant/`sr-only` content, (b) point at aria-label / aria-labelledby as
// the real naming mechanism, (c) correct the belief that real Chromium computes
// the name from content differently than jsdom (it does not — a jsdom
// `getByRole(role, { name })` miss is a REAL defect, not an artifact to work
// around), and (d) name the getByRole-by-name assertion that the mistake shows up in.
func assertCarriesA11yNameSteer(t *testing.T, p, label string) {
	t.Helper()
	for _, role := range []string{`role="status"`, `role="alert"`} {
		if !strings.Contains(p, role) {
			t.Errorf("%s does not name the live-region role %s", label, role)
		}
	}
	if !regexp.MustCompile(`(?i)aria-label`).MatchString(p) {
		t.Errorf("%s does not point at aria-label/aria-labelledby as the naming mechanism", label)
	}
	if !regexp.MustCompile(`(?i)sr-only|descendant`).MatchString(p) {
		t.Errorf("%s does not say the name is NOT taken from sr-only/descendant content", label)
	}
	if !regexp.MustCompile(`(?i)getByRole`).MatchString(p) {
		t.Errorf("%s does not name the getByRole(role, { name }) assertion the defect surfaces in", label)
	}
	// The core misconception to correct: jsdom and real Chromium agree here, so a
	// jsdom name miss is a real defect, not a jsdom limitation to defer to the browser.
	if !regexp.MustCompile(`(?i)chromium|browser`).MatchString(p) {
		t.Errorf("%s does not correct the jsdom-vs-real-browser belief", label)
	}
	if !regexp.MustCompile(`(?i)real defect|not a jsdom|same`).MatchString(p) {
		t.Errorf("%s does not say the jsdom name miss is a real defect (Chromium behaves the same)", label)
	}
}

// BEH-544: a ticket can be dispatched as live work after its fix already merged
// (often under a *sibling* ticket the host-side own-key guard can't catch). The
// prompt must steer the agent to verify the ticket's cited symbols/premise still
// hold before planning, and — if a grep shows the work already landed — to NOT
// fabricate a no-op change but record "already resolved, recommend close".
func TestBuildTddSteersToVerifyPremiseBeforePlanning(t *testing.T) {
	p := hImpl(t, sample, "beh-362")

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

// BEH-710: a /tdd session on the common "introduce a new reusable primitive +
// wire N call sites" feature shape is inherently read-heavy (research patterns,
// docs, and several call-site files before any code) and can exhaust the CONTEXT
// WINDOW — forcing an auto-compaction that may silently drop a RED→GREEN pairing
// or a partial multi-edit. Distinct from the wall-clock cap (BEH-688). The
// implementation prompt must steer the session to keep live context small:
// front-load research into a compact plan, release large file bodies once a call
// site is wired, and checkpoint-commit each vertical slice so a compaction has
// less live state to preserve.
func TestBuildTddSteersToManageContextBudgetOnReadHeavyWork(t *testing.T) {
	p := hImpl(t, sample, "beh-362")

	// Must name the context-window / auto-compaction risk (not the wall-clock cap).
	if !regexp.MustCompile(`(?i)context.{0,20}window`).MatchString(p) {
		t.Error("prompt does not name the context-window exhaustion risk")
	}
	if !regexp.MustCompile(`(?i)(auto-?)?compact`).MatchString(p) {
		t.Error("prompt does not mention auto-compaction as the failure mode")
	}
	// Must steer toward a compact, front-loaded plan and releasing large file bodies.
	if !regexp.MustCompile(`(?i)(front-?load|compact).{0,30}(plan|research)`).MatchString(p) {
		t.Error("prompt does not steer toward front-loading research into a compact plan")
	}
	if !regexp.MustCompile(`(?i)(stop holding|release|drop).{0,30}(file|source|context)`).MatchString(p) {
		t.Error("prompt does not steer toward releasing large file bodies from context")
	}
	// Must steer toward checkpoint-committing each vertical slice.
	if !regexp.MustCompile(`(?i)(checkpoint|commit).{0,40}(slice|call ?site)`).MatchString(p) {
		t.Error("prompt does not steer toward checkpoint-committing each vertical slice")
	}
}

// BEH-672: the /tdd session that hardened the Spinner's a11y (BEH-515) moved
// role="status" onto a wrapper and expected its accessible name to come from an
// `sr-only` child — then reasoned EXPLICITLY that a jsdom name miss was a jsdom
// limitation real Chromium would not share, and never ran the (in-sandbox
// unrunnable, BEH-477) browser story gate. It shipped a real defect. The
// implementation prompt must carry the steer that corrects this belief so the
// defect is reasoned about statically rather than deferred to a gate that cannot run.
func TestBuildTddCarriesA11yNameSteer(t *testing.T) {
	assertCarriesA11yNameSteer(t, hImpl(t, sample, "beh-362"), "tdd prompt")
}

// BEH-672: the resumed-branch prompt is a swap-in for BuildTdd, so a resumed
// implementation session that extends the branch's code needs the same
// accessible-name steer the fresh /tdd prompt carries.
func TestBuildTddResumedBranchCarriesA11yNameSteer(t *testing.T) {
	assertCarriesA11yNameSteer(t, hResumed(t, sample, "beh-362"), "tdd resumed-branch prompt")
}

// BEH-672: the resume prompt (re-entering an existing worktree) is likewise a
// BuildTdd swap-in and must carry the accessible-name steer.
func TestBuildTddResumeCarriesA11yNameSteer(t *testing.T) {
	assertCarriesA11yNameSteer(t, hResume(t, sample, "beh-362", sampleWorktree), "tdd resume prompt")
}

// BEH-672: the cold review that followed BEH-515 also missed the role="status"
// accessible-name defect — it verified the primitives with jsdom unit tests only
// and never ran the browser story gate (it can't in-sandbox, BEH-477). The review
// prompt must carry the same accessible-name steer so a reviewer treats a jsdom
// `getByRole(role, { name })` miss as a real defect to fix, not a jsdom artifact.
func TestBuildReviewCarriesA11yNameSteer(t *testing.T) {
	assertCarriesA11yNameSteer(t, hReview(t, sample, "beh-362", sampleWorktree), "review prompt")
}
