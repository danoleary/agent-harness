package filing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/linear"
)

// fakeFiler records the findings it was asked to file and replays scripted results.
type fakeFiler struct {
	calls   []findings.Finding
	results []fakeResult
}

type fakeResult struct {
	issue linear.CreatedIssue
	err   error
}

func (f *fakeFiler) FileFinding(fn findings.Finding, _ linear.FileFindingOptions) (linear.CreatedIssue, error) {
	i := len(f.calls)
	f.calls = append(f.calls, fn)
	if i < len(f.results) {
		return f.results[i].issue, f.results[i].err
	}
	return linear.CreatedIssue{}, nil
}

// recorder captures the narration events filing emits.
type recorder struct{ events []string }

func (r *recorder) Event(msg string) { r.events = append(r.events, msg) }

func writeDropbox(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.json"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write out.json: %v", err)
	}
}

func TestFileFilesEachFindingAndNarrates(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[
		{"title":"setup friction","body":"tokens:build not run","kind":"setup"},
		{"title":"opaque error","body":"vitest crash"}
	]`)

	filer := &fakeFiler{results: []fakeResult{
		{issue: linear.CreatedIssue{Identifier: "BEH-401"}},
		{issue: linear.CreatedIssue{Identifier: "BEH-402"}},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, rec)

	if len(filer.calls) != 2 {
		t.Fatalf("expected 2 findings filed, got %d", len(filer.calls))
	}
	if filer.calls[0].Title != "setup friction" {
		t.Errorf("first filed = %q", filer.calls[0].Title)
	}
	joined := strings.Join(rec.events, "\n")
	if !strings.Contains(joined, "BEH-401") || !strings.Contains(joined, "BEH-402") {
		t.Errorf("expected both filed identifiers narrated, got: %q", joined)
	}
}

// The common case: no session dropped a dropbox → file nothing, say nothing.
func TestFileIsSilentWhenNoDropbox(t *testing.T) {
	filer := &fakeFiler{}
	rec := &recorder{}

	File(t.TempDir(), "team-uuid", "BEH-370", filer, rec)

	if len(filer.calls) != 0 {
		t.Errorf("expected nothing filed, got %d", len(filer.calls))
	}
	if len(rec.events) != 0 {
		t.Errorf("expected no narration, got %v", rec.events)
	}
}

// Findings present but no team id resolved → file nothing, narrate why (we can't
// guess the team, and silently dropping findings would hide the gap).
func TestFileSkipsWhenNoTeamID(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"x","body":"y"}]`)
	filer := &fakeFiler{}
	rec := &recorder{}

	File(dir, "", "BEH-370", filer, rec)

	if len(filer.calls) != 0 {
		t.Errorf("expected nothing filed without a team id, got %d", len(filer.calls))
	}
	if len(rec.events) != 1 || !strings.Contains(rec.events[0], "no team id") {
		t.Errorf("expected one 'no team id' narration, got %v", rec.events)
	}
}

// A malformed dropbox is degraded to one narration line, never a crash.
func TestFileNarratesUnreadableDropbox(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `{not valid json`)
	filer := &fakeFiler{}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, rec)

	if len(filer.calls) != 0 {
		t.Errorf("expected nothing filed from a bad dropbox, got %d", len(filer.calls))
	}
	if len(rec.events) != 1 || !strings.Contains(rec.events[0], "unreadable") {
		t.Errorf("expected one 'unreadable' narration, got %v", rec.events)
	}
}

// One finding failing to file must not abort the rest.
func TestFileContinuesPastAFilingError(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"first","body":"a"},{"title":"second","body":"b"}]`)
	filer := &fakeFiler{results: []fakeResult{
		{err: errBoom},
		{issue: linear.CreatedIssue{Identifier: "BEH-402"}},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, rec)

	if len(filer.calls) != 2 {
		t.Fatalf("expected both findings attempted, got %d", len(filer.calls))
	}
	joined := strings.Join(rec.events, "\n")
	if !strings.Contains(joined, "failed to file") || !strings.Contains(joined, "BEH-402") {
		t.Errorf("expected a failure line and the second filed, got: %q", joined)
	}
}

// ClearDropbox removes a prior run's dropbox so it isn't re-filed: the findings
// dir is reused across runs of the same ticket, and the agent leaves out.json
// untouched when it has no findings. After a clear, File sees nothing to file.
func TestClearDropboxPreventsRefilingStaleFindings(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"from run 1","body":"a"}]`)

	if err := ClearDropbox(dir); err != nil {
		t.Fatalf("ClearDropbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.json")); !os.IsNotExist(err) {
		t.Fatalf("expected dropbox removed, stat err = %v", err)
	}

	// A second run that drops nothing must file nothing — not run 1's stale finding.
	filer := &fakeFiler{}
	rec := &recorder{}
	File(dir, "team-uuid", "BEH-370", filer, rec)
	if len(filer.calls) != 0 {
		t.Errorf("expected nothing filed after clear, got %d", len(filer.calls))
	}
	if len(rec.events) != 0 {
		t.Errorf("expected no narration after clear, got %v", rec.events)
	}
}

// Clearing a dir with no dropbox is a no-op, not an error (the common case: a
// first run, or a re-run after the prior run dropped nothing).
func TestClearDropboxIsNoOpWhenAbsent(t *testing.T) {
	if err := ClearDropbox(t.TempDir()); err != nil {
		t.Errorf("ClearDropbox on empty dir = %v, want nil", err)
	}
}

// DropboxExists is the retrospective tool's ground truth: it reports whether the
// session wrote out.json at all. It owns the dropbox filename so the truth check
// and the filer can never disagree on which file is the dropbox.
func TestDropboxExistsReportsPresence(t *testing.T) {
	dir := t.TempDir()
	if DropboxExists(dir) {
		t.Error("expected absent before the session writes the dropbox")
	}

	writeDropbox(t, dir, `[]`)
	if !DropboxExists(dir) {
		t.Error("expected present once out.json is written (even as [])")
	}
}

var errBoom = boomError("boom")

type boomError string

func (e boomError) Error() string { return string(e) }
