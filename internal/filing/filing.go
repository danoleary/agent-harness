// Package filing files the harness-improvement findings a session dropped into
// its `/findings/out.json` dropbox to the tracker, through the FindingsSink port
// (a Filer + Searcher; any tracker adapter satisfies it). It is the host-side step
// that runs after a session returns (ADR-0010, retaining ADR-0001's invariant: the
// harness owns all tracker I/O); the agent only writes the dropbox, never reaches
// the tracker itself.
package filing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/tracker"
)

// dropboxFile is the fixed filename the agent drops findings into, under the
// session's findings dir (mounted at /findings in the sandbox).
const dropboxFile = "out.json"

// Filer files one finding to the tracker. Any tracker adapter satisfies it; tests pass a fake.
type Filer interface {
	FileFinding(f findings.Finding, opts tracker.FileFindingOptions) (tracker.CreatedIssue, error)
}

// Searcher lists already-filed harness findings for a team so File can skip
// duplicates. Any tracker adapter satisfies it; tests pass a fake.
type Searcher interface {
	SearchFindings(teamID string) ([]tracker.ExistingFinding, error)
}

// ClearDropbox removes any stale dropbox left in findingsDir. The findings dir is
// ticket+session keyed (not run-id keyed), so a re-run of the same ticket reuses
// it — and the prompt tells the agent to leave out.json untouched when it has no
// findings. Without this, a prior run's dropbox would be re-filed as duplicate
// issues. Call it before each session so File only ever sees the current run's
// drop. An absent dropbox is a no-op.
func ClearDropbox(findingsDir string) error {
	if err := os.Remove(filepath.Join(findingsDir, dropboxFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ClearEmptyDropbox removes the dropbox iff it is present and cleanly parses to
// zero findings — the skill's default `[]` (BEH-568). It exists for the
// spending-cap-abort path: the retrospective writes out.json as `[]` up front
// (BEH-536's incremental write), so a session capped mid-run leaves that default
// `[]`. Left in place it reads as "ran, found nothing" (DropboxExists → true),
// masking the abort and suppressing the retry-after-reset. Removing only the
// empty default restores the "absent file = never ran" contract while preserving
// any real findings a session managed to write before the cap fired. A
// malformed or non-empty dropbox is left untouched (returns false); an absent
// dropbox is a no-op. Returns whether it removed the file.
func ClearEmptyDropbox(findingsDir string) (bool, error) {
	path := filepath.Join(findingsDir, dropboxFile)
	text, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	parsed := findings.Parse(string(text))
	if parsed.Error != "" || len(parsed.Findings) > 0 {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	return true, nil
}

// DropboxExists reports whether a session wrote its findings dropbox
// (`out.json`) in findingsDir. It is the retrospective tool's ground truth: an
// absent file means the retrospective step never ran (DESIGN.md). It owns the
// dropbox filename so the truth check and File can never disagree on which file
// is the dropbox.
func DropboxExists(findingsDir string) bool {
	_, err := os.Stat(filepath.Join(findingsDir, dropboxFile))
	return err == nil
}

// EventSink receives the concise narration filing emits (the runlog.Logger).
type EventSink interface {
	Event(msg string)
}

// SemanticMatcher decides whether a finding is the same root-cause class as one
// of the already-open findings, returning that issue's identifier or "" for none.
// It exists because matchKey only catches exact key/title equality, while most
// real duplicates are the same class described in different prose (BEH-573): two
// sessions word the same failure differently and pick different free-form keys,
// so neither the key nor the title collides. The semantic matcher is a separate
// collaborator (internal/semdedup); tests pass a fake. Best-effort (ADR-0001): the caller treats any error as "no
// match" and degrades to filing the finding, the pre-semantic behaviour.
type SemanticMatcher interface {
	MatchFinding(f findings.Finding, open []tracker.ExistingFinding) (identifier string, err error)
}

// OccurrenceRecorder records that an already-tracked finding recurred: it appends
// an occurrence comment to the existing issue and bumps its occurrence count
// (BEH-573), turning N duplicate files into one issue carrying N occurrences —
// also a better prioritisation signal than N near-identical issues. It returns
// the new count for narration. Best-effort (ADR-0001): the caller narrates a
// failure and still skips the duplicate, never crashing or filing a dup.
type OccurrenceRecorder interface {
	RecordOccurrence(identifier, relatedIdentifier string) (count int, err error)
}

// File reads the findings dropbox in findingsDir and files each finding to the
// tracker, referencing relatedIdentifier (the worked ticket). It never returns an
// error: every failure is degraded to a narration line, so one bad dropbox or a
// transient tracker failure can't crash the run (ADR-0001: degraded, not broken).
//   - No dropbox file → nothing to file (the common, friction-free case).
//   - Dropbox present but unreadable/malformed → one narration line, nothing filed.
//   - Findings present but no team id resolved → one narration line, nothing filed.
//
// Dedup runs in two passes before filing: the fast exact key/title match
// (matchKey), then — for anything that misses — a best-effort semantic pass via
// matcher. A match (either pass) is recorded as a recurrence on the existing
// issue (comment-and-bump) instead of filing a duplicate. matcher and recorder
// are both optional: nil matcher skips the semantic pass, nil recorder degrades a
// match to the pre-BEH-573 silent-skip narration — so a caller can opt into
// either half independently and a failure of either degrades, never crashes.
func File(findingsDir, teamID, relatedIdentifier string, filer Filer, searcher Searcher, matcher SemanticMatcher, recorder OccurrenceRecorder, log EventSink) {
	parsed, ok := readDropbox(findingsDir, log)
	if !ok {
		return
	}
	fileToTracker(parsed.Findings, teamID, relatedIdentifier, filer, searcher, matcher, recorder, log)
}

// Route reads the findings dropbox once and routes each finding by its audience
// (ADR-0011): harness findings write to harnessDir (the local artifact — nothing
// leaves the repo without the opt-in that lands in BEH-640), and project findings
// file to the tracker through the same dedup pipeline File uses. It is the
// audience-aware successor to File the stages call; File remains the plain
// file-everything-to-the-tracker primitive. Best-effort throughout (ADR-0001):
// every failure degrades to narration, never a crash.
func Route(findingsDir, harnessDir, teamID, relatedIdentifier string, filer Filer, searcher Searcher, matcher SemanticMatcher, recorder OccurrenceRecorder, log EventSink) {
	parsed, ok := readDropbox(findingsDir, log)
	if !ok {
		return
	}
	project, harness := partitionByAudience(parsed.Findings)
	WriteHarnessFindings(harnessDir, relatedIdentifier, harness, log)
	fileToTracker(project, teamID, relatedIdentifier, filer, searcher, matcher, recorder, log)
}

// readDropbox reads and parses the dropbox in findingsDir. ok is false — with a
// narration only on a real parse error — when there is nothing to act on: an
// absent file (the common, friction-free case) or a malformed one.
func readDropbox(findingsDir string, log EventSink) (findings.Parsed, bool) {
	text, err := os.ReadFile(filepath.Join(findingsDir, dropboxFile))
	if err != nil {
		return findings.Parsed{}, false // no dropbox file → nothing to file
	}
	parsed := findings.Parse(string(text))
	if parsed.Error != "" {
		log.Event("findings ✗ dropbox unreadable: " + parsed.Error)
		return findings.Parsed{}, false
	}
	return parsed, true
}

// partitionByAudience splits findings into the project-audience set (→ tracker)
// and the harness-audience set (→ local dir). An unclassified finding already
// carries AudienceHarness from Parse, so it lands in the harness set.
func partitionByAudience(fs []findings.Finding) (project, harness []findings.Finding) {
	for _, f := range fs {
		if f.Audience == findings.AudienceProject {
			project = append(project, f)
		} else {
			harness = append(harness, f)
		}
	}
	return project, harness
}

// fileToTracker files the given findings to the tracker with cross-run dedup. It
// is the shared engine behind File (all findings) and Route (project findings
// only). An empty set is a no-op; a non-empty set with no team id resolved is one
// narration line and nothing filed.
func fileToTracker(fs []findings.Finding, teamID, relatedIdentifier string, filer Filer, searcher Searcher, matcher SemanticMatcher, recorder OccurrenceRecorder, log EventSink) {
	if len(fs) == 0 {
		return
	}
	if teamID == "" {
		log.Event(fmt.Sprintf(
			"findings ✗ %d dropped but no team id resolved for %s",
			len(fs), relatedIdentifier,
		))
		return
	}

	// Look up findings already tracked so we don't re-file duplicates across runs.
	// Best-effort (ADR-0001): a search failure degrades to filing everything (an
	// empty tracked map AND no open set, so the semantic pass is also skipped) —
	// the pre-dedup behaviour, never a crash.
	tracked, open := openTracked(teamID, searcher, log)

	for _, f := range fs {
		// 1. Exact match — the fast short-circuit, preserved ahead of the model.
		if existing, ok := tracked[matchKey(f.Key, f.Title)]; ok {
			recordRecurrence(existing, relatedIdentifier, recorder, log)
			continue
		}
		// 2. Semantic match — best-effort; an error, a nil matcher, an empty open
		// set, or an unrecognised id all fall through to filing as new.
		if existing, ok := semanticMatch(f, open, matcher, log); ok {
			recordRecurrence(existing, relatedIdentifier, recorder, log)
			// Register so a later same-class finding in this dropbox also dedups
			// against it (its exact key/title now points at the matched issue).
			tracked[matchKey(f.Key, f.Title)] = existing
			continue
		}
		// 3. No match — file a new issue.
		created, err := filer.FileFinding(f, tracker.FileFindingOptions{
			TeamID: teamID, RelatedKey: relatedIdentifier,
		})
		if err != nil {
			log.Event(fmt.Sprintf("finding ✗ failed to file %q: %s", f.Title, err.Error()))
			continue
		}
		// Register what we just filed so a later finding in the same dropbox that
		// shares its key/title — or reads as the same class — dedups against it too,
		// not just across runs.
		filed := tracker.ExistingFinding{Identifier: created.Identifier, Title: f.Title, Key: f.Key}
		tracked[matchKey(f.Key, f.Title)] = filed
		open = append(open, filed)
		log.Event(fmt.Sprintf("finding filed: %s — %s", created.Identifier, f.Title))
	}
}

// recordRecurrence handles a finding that matched an already-tracked issue:
// comment-and-bump when a recorder is wired (BEH-573), else the pre-BEH-573
// silent-skip narration. Best-effort: a recorder failure degrades to a skip
// narration so a transient tracker error never files a duplicate or crashes.
func recordRecurrence(existing tracker.ExistingFinding, relatedIdentifier string, recorder OccurrenceRecorder, log EventSink) {
	if recorder == nil {
		log.Event(fmt.Sprintf("finding ↩ already tracked: %s — %s", existing.Identifier, existing.Title))
		return
	}
	count, err := recorder.RecordOccurrence(existing.Identifier, relatedIdentifier)
	if err != nil {
		log.Event(fmt.Sprintf("finding ⚠ recurrence not recorded on %s (skipped, not re-filed): %s", existing.Identifier, err.Error()))
		return
	}
	log.Event(fmt.Sprintf("finding ↩ recurred (occurrence %d): %s — %s", count, existing.Identifier, existing.Title))
}

// semanticMatch asks the matcher whether f duplicates one of the open findings,
// resolving the returned identifier back to its ExistingFinding. Best-effort: a
// nil matcher, an empty open set, a model error, or an identifier not in the open
// set all yield ok=false so the finding is filed as new (the pre-semantic
// behaviour) rather than mis-deduped against an issue we can't verify.
func semanticMatch(f findings.Finding, open []tracker.ExistingFinding, matcher SemanticMatcher, log EventSink) (tracker.ExistingFinding, bool) {
	if matcher == nil || len(open) == 0 {
		return tracker.ExistingFinding{}, false
	}
	id, err := matcher.MatchFinding(f, open)
	if err != nil {
		log.Event("finding ⚠ semantic dedup failed, filing without it: " + err.Error())
		return tracker.ExistingFinding{}, false
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return tracker.ExistingFinding{}, false
	}
	for _, e := range open {
		if e.Identifier == id {
			return e, true
		}
	}
	// The model named an id that isn't in the open set we gave it — don't trust a
	// match we can't resolve; file as new.
	log.Event("finding ⚠ semantic dedup returned unknown id " + id + ", filing as new")
	return tracker.ExistingFinding{}, false
}

// openTracked returns the already-filed harness findings both keyed by match key
// (for the exact short-circuit) and as an ordered slice (the candidate set for
// the semantic pass), limited to OPEN issues — a closed/canceled match must not
// suppress a re-file. A search failure is narrated once and degrades to an empty
// map and nil slice (file all, no semantic pass).
func openTracked(teamID string, searcher Searcher, log EventSink) (map[string]tracker.ExistingFinding, []tracker.ExistingFinding) {
	tracked := map[string]tracker.ExistingFinding{}
	existing, err := searcher.SearchFindings(teamID)
	if err != nil {
		log.Event("findings ⚠ dedup search failed, filing without dedup: " + err.Error())
		return tracked, nil
	}
	open := make([]tracker.ExistingFinding, 0, len(existing))
	for _, e := range existing {
		if e.Closed {
			continue
		}
		tracked[matchKey(e.Key, e.Title)] = e
		open = append(open, e)
	}
	return tracked, open
}

// PriorFinding is an already-known harness finding class (its dedup key and
// human title) surfaced to a retrospective re-run so the session treats it as
// settled instead of re-deriving it (BEH-539). Key may be empty for a finding
// that was only ever title-keyed.
type PriorFinding struct {
	Key   string
	Title string
}

// AlreadyFiled returns the deduped set of finding classes a retrospective re-run
// should treat as settled (BEH-539): the team's OPEN filed findings, merged with
// any classes left in this ticket's prior dropbox. It MUST be called before
// ClearDropbox wipes that dropbox. Best-effort (ADR-0001): a tracker search
// failure degrades to the dropbox classes alone and narrates, never crashes. The
// result is sorted (key, then title) so the injected prompt is deterministic.
func AlreadyFiled(findingsDir, teamID string, searcher Searcher, log EventSink) []PriorFinding {
	seen := map[string]bool{}
	out := []PriorFinding{}
	add := func(key, title string) {
		k := matchKey(key, title)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, PriorFinding{Key: key, Title: title})
	}

	// The team's open filed findings — the authoritative set of settled classes.
	if existing, err := searcher.SearchFindings(teamID); err != nil {
		log.Event("retrospective context ⚠ already-filed lookup failed, prior-finding context limited to the dropbox: " + err.Error())
	} else {
		for _, e := range existing {
			if e.Closed {
				continue
			}
			add(e.Key, e.Title)
		}
	}

	// Any classes the prior run dropped but that may never have reached the tracker
	// (e.g. a transient filing failure). Read here, before ClearDropbox wipes it.
	if text, err := os.ReadFile(filepath.Join(findingsDir, dropboxFile)); err == nil {
		for _, f := range findings.Parse(string(text)).Findings {
			add(f.Key, f.Title)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Title < out[j].Title
	})
	return out
}

// matchKey is the dedup fingerprint: the explicit failure-class key when set,
// else a normalized form of the title (lowercased, trimmed) as a fallback.
func matchKey(key, title string) string {
	if k := strings.TrimSpace(key); k != "" {
		return "key:" + strings.ToLower(k)
	}
	return "title:" + strings.ToLower(strings.TrimSpace(title))
}
