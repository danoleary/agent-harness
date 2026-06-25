package filing

import (
	"os"
	"path/filepath"
	"reflect"
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

// fakeSearcher replays a scripted set of already-filed findings (or an error).
type fakeSearcher struct {
	existing []linear.ExistingFinding
	err      error
	calls    int
}

func (s *fakeSearcher) SearchFindings(_ string) ([]linear.ExistingFinding, error) {
	s.calls++
	return s.existing, s.err
}

// noExisting is a searcher that finds nothing already tracked (the first-run case).
func noExisting() *fakeSearcher { return &fakeSearcher{} }

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

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), rec)

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

// Dedup: a finding whose key already has an OPEN issue is not filed again —
// it's narrated as already tracked instead. This is the core BEH-410 behaviour.
func TestFileSkipsFindingAlreadyTrackedByOpenIssue(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"Playwright can't run in sandbox","body":"missing deps","key":"sandbox-playwright-missing-deps"}]`)

	filer := &fakeFiler{}
	searcher := &fakeSearcher{existing: []linear.ExistingFinding{
		{Identifier: "BEH-405", Title: "Storybook unrunnable", Key: "sandbox-playwright-missing-deps"},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, rec)

	if len(filer.calls) != 0 {
		t.Fatalf("expected the duplicate to be skipped, but %d were filed", len(filer.calls))
	}
	joined := strings.Join(rec.events, "\n")
	if !strings.Contains(joined, "already tracked") || !strings.Contains(joined, "BEH-405") {
		t.Errorf("expected an 'already tracked' narration naming BEH-405, got: %q", joined)
	}
}

// Dedup keys off OPEN issues only: a closed/canceled match must NOT suppress a
// re-file, or a wontfix would permanently mask a real regression after a fix ships.
func TestFileRefilesWhenOnlyMatchIsClosed(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"Playwright regressed again","body":"deps missing","key":"sandbox-playwright-missing-deps"}]`)

	filer := &fakeFiler{results: []fakeResult{{issue: linear.CreatedIssue{Identifier: "BEH-500"}}}}
	searcher := &fakeSearcher{existing: []linear.ExistingFinding{
		{Identifier: "BEH-405", Title: "old one", Key: "sandbox-playwright-missing-deps", Closed: true},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, rec)

	if len(filer.calls) != 1 {
		t.Fatalf("expected the finding re-filed past the closed match, got %d filed", len(filer.calls))
	}
	joined := strings.Join(rec.events, "\n")
	if !strings.Contains(joined, "BEH-500") {
		t.Errorf("expected the new issue narrated, got: %q", joined)
	}
}

// Fallback dedup: when a finding carries no explicit key, its normalized title
// is the match key, so a re-surfaced finding with the same title still dedups.
func TestFileDedupsOnTitleWhenNoKey(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"Build OOM-killed at prerender","body":"exit 137"}]`)

	filer := &fakeFiler{}
	searcher := &fakeSearcher{existing: []linear.ExistingFinding{
		{Identifier: "BEH-407", Title: "build oom-killed at prerender  "},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, rec)

	if len(filer.calls) != 0 {
		t.Fatalf("expected the same-title finding deduped, got %d filed", len(filer.calls))
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "already tracked") {
		t.Errorf("expected an 'already tracked' narration, got: %v", rec.events)
	}
}

// Dedup is best-effort (ADR-0001): a search failure must degrade to the
// pre-dedup behaviour — file everything, narrate the degradation, never crash.
func TestFileFilesAllWhenSearchFails(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"a","body":"x"},{"title":"b","body":"y"}]`)

	filer := &fakeFiler{}
	searcher := &fakeSearcher{err: errBoom}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, rec)

	if len(filer.calls) != 2 {
		t.Fatalf("expected both findings filed despite the search failure, got %d", len(filer.calls))
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "dedup search failed") {
		t.Errorf("expected a degraded-dedup narration, got: %v", rec.events)
	}
}

// Within a single dropbox, two findings sharing a key collapse to one filed
// issue — the same dedup applies in-run, not just across runs.
func TestFileDedupsWithinASingleRun(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[
		{"title":"first wording","body":"a","key":"same-class"},
		{"title":"second wording","body":"b","key":"same-class"}
	]`)

	filer := &fakeFiler{results: []fakeResult{{issue: linear.CreatedIssue{Identifier: "BEH-600"}}}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), rec)

	if len(filer.calls) != 1 {
		t.Fatalf("expected the two same-key findings collapsed to one filed, got %d", len(filer.calls))
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "already tracked") {
		t.Errorf("expected the second to be narrated as already tracked, got: %v", rec.events)
	}
}

// The common case: no session dropped a dropbox → file nothing, say nothing.
func TestFileIsSilentWhenNoDropbox(t *testing.T) {
	filer := &fakeFiler{}
	rec := &recorder{}

	File(t.TempDir(), "team-uuid", "BEH-370", filer, noExisting(), rec)

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

	File(dir, "", "BEH-370", filer, noExisting(), rec)

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

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), rec)

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

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), rec)

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
	File(dir, "team-uuid", "BEH-370", filer, noExisting(), rec)
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

// AlreadyFiled feeds a retrospective re-run the set of settled finding classes
// (BEH-539). It returns the team's OPEN filed findings as {key, title}, skipping
// closed ones (a closed finding is no longer a settled class to avoid).
func TestAlreadyFiledReturnsOpenFindingsSkippingClosed(t *testing.T) {
	searcher := &fakeSearcher{existing: []linear.ExistingFinding{
		{Identifier: "BEH-529", Title: "Build OOMs", Key: "sandbox-build-oom"},
		{Identifier: "BEH-100", Title: "old closed one", Key: "ancient-class", Closed: true},
	}}
	rec := &recorder{}

	got := AlreadyFiled(t.TempDir(), "team-uuid", searcher, rec)

	if len(got) != 1 {
		t.Fatalf("expected only the open finding, got %d: %+v", len(got), got)
	}
	if got[0].Key != "sandbox-build-oom" || got[0].Title != "Build OOMs" {
		t.Errorf("unexpected entry: %+v", got[0])
	}
}

// The prior run's dropbox is read (before ClearDropbox wipes it) so classes that
// never reached Linear still count as settled — merged with the Linear set and
// deduped by key so a class in both sources appears once.
func TestAlreadyFiledMergesPriorDropboxAndDedupsByKey(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[
		{"title":"Build OOMs","body":"x","key":"sandbox-build-oom"},
		{"title":"Storybook OOMs","body":"y","key":"sandbox-storybook-oom"}
	]`)
	searcher := &fakeSearcher{existing: []linear.ExistingFinding{
		{Identifier: "BEH-529", Title: "Build OOMs", Key: "sandbox-build-oom"},
	}}

	got := AlreadyFiled(dir, "team-uuid", searcher, &recorder{})

	keys := map[string]int{}
	for _, f := range got {
		keys[f.Key]++
	}
	if keys["sandbox-build-oom"] != 1 {
		t.Errorf("expected the shared class deduped to one, got %d (%+v)", keys["sandbox-build-oom"], got)
	}
	if keys["sandbox-storybook-oom"] != 1 {
		t.Errorf("expected the dropbox-only class included once, got %d (%+v)", keys["sandbox-storybook-oom"], got)
	}
	if len(got) != 2 {
		t.Errorf("expected exactly two classes after dedup, got %d: %+v", len(got), got)
	}
}

// Best-effort (ADR-0001): a Linear search failure must degrade to the dropbox
// classes alone and narrate, never crash.
func TestAlreadyFiledDegradesToDropboxWhenSearchFails(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"Build OOMs","body":"x","key":"sandbox-build-oom"}]`)
	searcher := &fakeSearcher{err: errBoom}
	rec := &recorder{}

	got := AlreadyFiled(dir, "team-uuid", searcher, rec)

	if len(got) != 1 || got[0].Key != "sandbox-build-oom" {
		t.Fatalf("expected the dropbox class despite the search failure, got %+v", got)
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "failed") {
		t.Errorf("expected a degraded-lookup narration, got %v", rec.events)
	}
}

// The result is sorted (key, then title) so the injected prompt is
// deterministic across runs regardless of the order Linear returns findings in.
func TestAlreadyFiledSortsDeterministically(t *testing.T) {
	searcher := &fakeSearcher{existing: []linear.ExistingFinding{
		{Identifier: "BEH-3", Title: "Z last", Key: "zzz-class"},
		{Identifier: "BEH-1", Title: "A first", Key: "aaa-class"},
		{Identifier: "BEH-2", Title: "mmm title only"},
	}}

	got := AlreadyFiled(t.TempDir(), "team-uuid", searcher, &recorder{})

	gotKeys := make([]string, len(got))
	for i, f := range got {
		gotKeys[i] = f.Key
	}
	// Empty key (title-only) sorts before any explicit key, then key ascending.
	want := []string{"", "aaa-class", "zzz-class"}
	if !reflect.DeepEqual(gotKeys, want) {
		t.Errorf("AlreadyFiled not sorted deterministically: got %v, want %v", gotKeys, want)
	}
}

// First run: nothing filed and no prior dropbox → empty, no narration.
func TestAlreadyFiledIsEmptyOnFirstRun(t *testing.T) {
	rec := &recorder{}
	got := AlreadyFiled(t.TempDir(), "team-uuid", noExisting(), rec)
	if len(got) != 0 {
		t.Errorf("expected nothing already filed on a first run, got %+v", got)
	}
	if len(rec.events) != 0 {
		t.Errorf("expected no narration on a clean first run, got %v", rec.events)
	}
}

var errBoom = boomError("boom")

type boomError string

func (e boomError) Error() string { return string(e) }
