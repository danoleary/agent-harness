package filing

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/tracker"
)

// fakeFiler records the findings it was asked to file and replays scripted results.
type fakeFiler struct {
	calls   []findings.Finding
	results []fakeResult
}

type fakeResult struct {
	issue tracker.CreatedIssue
	err   error
}

func (f *fakeFiler) FileFinding(fn findings.Finding, _ tracker.FileFindingOptions) (tracker.CreatedIssue, error) {
	i := len(f.calls)
	f.calls = append(f.calls, fn)
	if i < len(f.results) {
		return f.results[i].issue, f.results[i].err
	}
	return tracker.CreatedIssue{}, nil
}

// fakeSearcher replays a scripted set of already-filed findings (or an error).
type fakeSearcher struct {
	existing []tracker.ExistingFinding
	err      error
	calls    int
}

func (s *fakeSearcher) SearchFindings(_ string) ([]tracker.ExistingFinding, error) {
	s.calls++
	return s.existing, s.err
}

// noExisting is a searcher that finds nothing already tracked (the first-run case).
func noExisting() *fakeSearcher { return &fakeSearcher{} }

// fakeMatcher replays a scripted semantic verdict: the identifier it matches f to
// (or "" for none), or an error. It records the open set it was handed so a test
// can assert the candidate list was passed through.
type fakeMatcher struct {
	matchTo  string
	err      error
	calls    int
	lastOpen []tracker.ExistingFinding
}

func (m *fakeMatcher) MatchFinding(_ findings.Finding, open []tracker.ExistingFinding) (string, error) {
	m.calls++
	m.lastOpen = open
	return m.matchTo, m.err
}

// fakeRecorder records which issues were bumped as recurrences and replays a
// scripted count/error.
type fakeRecorder struct {
	bumped []string
	count  int
	err    error
}

func (r *fakeRecorder) RecordOccurrence(identifier, _ string) (int, error) {
	r.bumped = append(r.bumped, identifier)
	return r.count, r.err
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
		{issue: tracker.CreatedIssue{Identifier: "BEH-401"}},
		{issue: tracker.CreatedIssue{Identifier: "BEH-402"}},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), nil, nil, rec)

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
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "Storybook unrunnable", Key: "sandbox-playwright-missing-deps"},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, nil, nil, rec)

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

	filer := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "BEH-500"}}}}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "old one", Key: "sandbox-playwright-missing-deps", Closed: true},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, nil, nil, rec)

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
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-407", Title: "build oom-killed at prerender  "},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, nil, nil, rec)

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

	File(dir, "team-uuid", "BEH-370", filer, searcher, nil, nil, rec)

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

	filer := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "BEH-600"}}}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), nil, nil, rec)

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

	File(t.TempDir(), "team-uuid", "BEH-370", filer, noExisting(), nil, nil, rec)

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

	File(dir, "", "BEH-370", filer, noExisting(), nil, nil, rec)

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

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), nil, nil, rec)

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
		{issue: tracker.CreatedIssue{Identifier: "BEH-402"}},
	}}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, noExisting(), nil, nil, rec)

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
	File(dir, "team-uuid", "BEH-370", filer, noExisting(), nil, nil, rec)
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

// ClearEmptyDropbox removes a present-but-empty default `[]` dropbox so the
// "absent file = never ran" contract holds after a spending-cap abort: the
// retrospective skill writes out.json as `[]` up front (BEH-536's incremental
// write), so a session capped mid-run leaves that default `[]`, which would
// otherwise read as "ran, found nothing" and suppress the retry (BEH-568).
func TestClearEmptyDropboxRemovesDefaultEmptyArray(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[]`)

	removed, err := ClearEmptyDropbox(dir)
	if err != nil {
		t.Fatalf("ClearEmptyDropbox: %v", err)
	}
	if !removed {
		t.Error("expected removed = true for a default [] dropbox")
	}
	if DropboxExists(dir) {
		t.Error("expected dropbox removed so DropboxExists is false (never-ran contract)")
	}
}

// A spending-cap abort may strike after the session wrote real findings
// incrementally — those must survive so the harness still files them. Only the
// empty default is cleared.
func TestClearEmptyDropboxPreservesRealFindings(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"real friction","body":"detail"}]`)

	removed, err := ClearEmptyDropbox(dir)
	if err != nil {
		t.Fatalf("ClearEmptyDropbox: %v", err)
	}
	if removed {
		t.Error("expected removed = false when the dropbox carries real findings")
	}
	if !DropboxExists(dir) {
		t.Error("expected the dropbox with real findings to be kept")
	}
}

// Clearing an empty dropbox in a dir that has none is a no-op (an abort that
// never started never wrote one).
func TestClearEmptyDropboxIsNoOpWhenAbsent(t *testing.T) {
	removed, err := ClearEmptyDropbox(t.TempDir())
	if err != nil {
		t.Errorf("ClearEmptyDropbox on empty dir = %v, want nil", err)
	}
	if removed {
		t.Error("expected removed = false when there is no dropbox")
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
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
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
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
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
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
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

// The headline BEH-573 behaviour: a finding whose prose and key DON'T match any
// open issue exactly, but which the semantic matcher reports is the same class,
// is NOT filed as a new issue — the matched issue is bumped as a recurrence.
func TestFileSemanticMatchBumpsInsteadOfFiling(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"review aborted on spend cap yet PR still opened","body":"no verdict","key":"review-spendcap-abort-pushes-pr-without-verdict"}]`)

	filer := &fakeFiler{}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-572", Title: "spending-cap-aborted review still pushes a PR", Key: "review-spending-cap-abort-still-pushes-pr-without-qualitative-review"},
	}}
	matcher := &fakeMatcher{matchTo: "BEH-572"}
	bumper := &fakeRecorder{count: 2}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, matcher, bumper, rec)

	if len(filer.calls) != 0 {
		t.Fatalf("expected the reworded duplicate NOT filed, but %d were filed", len(filer.calls))
	}
	if len(bumper.bumped) != 1 || bumper.bumped[0] != "BEH-572" {
		t.Fatalf("expected BEH-572 bumped once as a recurrence, got %v", bumper.bumped)
	}
	joined := strings.Join(rec.events, "\n")
	if !strings.Contains(joined, "recurred") || !strings.Contains(joined, "BEH-572") {
		t.Errorf("expected a recurrence narration naming BEH-572, got: %q", joined)
	}
}

// With a recorder wired, an EXACT key match is also recorded as a recurrence
// (comment-and-bump), not the silent skip — and the fast exact short-circuit is
// preserved, so the semantic matcher is never consulted for an exact hit.
func TestFileExactMatchBumpsAndSkipsSemanticPass(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"Playwright can't run in sandbox","body":"missing deps","key":"sandbox-playwright-missing-deps"}]`)

	filer := &fakeFiler{}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "Storybook unrunnable", Key: "sandbox-playwright-missing-deps"},
	}}
	matcher := &fakeMatcher{matchTo: "BEH-999"} // would mis-match if ever consulted
	bumper := &fakeRecorder{count: 3}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, matcher, bumper, rec)

	if len(filer.calls) != 0 {
		t.Fatalf("expected exact dup not filed, got %d filed", len(filer.calls))
	}
	if matcher.calls != 0 {
		t.Errorf("expected the matcher NOT consulted on an exact hit (short-circuit), got %d calls", matcher.calls)
	}
	if len(bumper.bumped) != 1 || bumper.bumped[0] != "BEH-405" {
		t.Fatalf("expected BEH-405 bumped on the exact match, got %v", bumper.bumped)
	}
}

// No-match: when neither the exact key/title nor the semantic matcher matches, the
// finding is filed as a new issue (the matcher returning "" must not block it).
func TestFileFilesNewWhenSemanticReturnsNoMatch(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"a brand new failure class","body":"x"}]`)

	filer := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "BEH-700"}}}}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "unrelated open finding"},
	}}
	matcher := &fakeMatcher{matchTo: ""} // explicitly "none of these"
	bumper := &fakeRecorder{}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, matcher, bumper, rec)

	if len(filer.calls) != 1 {
		t.Fatalf("expected the unmatched finding filed as new, got %d filed", len(filer.calls))
	}
	if len(bumper.bumped) != 0 {
		t.Errorf("expected nothing bumped on a no-match, got %v", bumper.bumped)
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "BEH-700") {
		t.Errorf("expected the new issue narrated, got %v", rec.events)
	}
}

// Best-effort (ADR-0001): a semantic-matcher ERROR must degrade to filing the
// finding (the pre-semantic behaviour) and narrate, never crash or silently drop.
func TestFileFilesWhenSemanticMatcherErrors(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"some finding","body":"x"}]`)

	filer := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "BEH-701"}}}}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "an open finding"},
	}}
	matcher := &fakeMatcher{err: errBoom}
	bumper := &fakeRecorder{}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, matcher, bumper, rec)

	if len(filer.calls) != 1 {
		t.Fatalf("expected the finding filed despite the matcher error, got %d filed", len(filer.calls))
	}
	if len(bumper.bumped) != 0 {
		t.Errorf("expected nothing bumped when the matcher errored, got %v", bumper.bumped)
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "semantic dedup failed") {
		t.Errorf("expected a degraded-semantic narration, got %v", rec.events)
	}
}

// Best-effort: a recorder failure on a confirmed match must NOT fall back to
// filing a duplicate — the match still held, so the finding is skipped with a
// degraded narration. Re-filing on a transient comment error is the worse outcome.
func TestFileSkipsWithoutRefilingWhenRecorderErrors(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"dup","body":"x","key":"same-class"}]`)

	filer := &fakeFiler{}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "tracked", Key: "same-class"},
	}}
	bumper := &fakeRecorder{err: errBoom}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, nil, bumper, rec)

	if len(filer.calls) != 0 {
		t.Fatalf("expected no duplicate filed when the recorder errors, got %d filed", len(filer.calls))
	}
	if !strings.Contains(strings.Join(rec.events, "\n"), "recurrence not recorded") {
		t.Errorf("expected a degraded-recorder narration, got %v", rec.events)
	}
}

// The matcher is handed the open candidate set to compare against (the thing that
// lets it spot a same-class recurrence at all).
func TestFilePassesOpenFindingsToMatcher(t *testing.T) {
	dir := t.TempDir()
	writeDropbox(t, dir, `[{"title":"new wording","body":"x"}]`)

	filer := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "BEH-702"}}}}
	searcher := &fakeSearcher{existing: []tracker.ExistingFinding{
		{Identifier: "BEH-405", Title: "open one", Closed: false},
		{Identifier: "BEH-406", Title: "closed one", Closed: true},
	}}
	matcher := &fakeMatcher{matchTo: ""}
	rec := &recorder{}

	File(dir, "team-uuid", "BEH-370", filer, searcher, matcher, &fakeRecorder{}, rec)

	if matcher.calls != 1 {
		t.Fatalf("expected the matcher consulted once, got %d", matcher.calls)
	}
	if len(matcher.lastOpen) != 1 || matcher.lastOpen[0].Identifier != "BEH-405" {
		t.Errorf("expected only the OPEN finding passed as a candidate, got %+v", matcher.lastOpen)
	}
}

var errBoom = boomError("boom")

type boomError string

func (e boomError) Error() string { return string(e) }
