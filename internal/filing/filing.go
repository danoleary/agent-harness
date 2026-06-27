// Package filing files the harness-improvement findings a session dropped into
// its `/findings/out.json` dropbox to Linear. It is the host-side step that runs
// after a session returns (ADR-0001: the harness owns all Linear I/O); the
// agent only writes the dropbox, never reaches Linear itself.
package filing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/linear"
)

// dropboxFile is the fixed filename the agent drops findings into, under the
// session's findings dir (mounted at /findings in the sandbox).
const dropboxFile = "out.json"

// Filer files one finding to Linear. *linear.Client satisfies it; tests pass a fake.
type Filer interface {
	FileFinding(f findings.Finding, opts linear.FileFindingOptions) (linear.CreatedIssue, error)
}

// Searcher lists already-filed harness findings for a team so File can skip
// duplicates. *linear.Client satisfies it; tests pass a fake.
type Searcher interface {
	SearchFindings(teamID string) ([]linear.ExistingFinding, error)
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

// File reads the findings dropbox in findingsDir and files each finding to
// Linear, referencing relatedIdentifier (the worked ticket). It never returns an
// error: every failure is degraded to a narration line, so one bad dropbox or a
// transient Linear failure can't crash the run (ADR-0001: degraded, not broken).
//   - No dropbox file → nothing to file (the common, friction-free case).
//   - Dropbox present but unreadable/malformed → one narration line, nothing filed.
//   - Findings present but no team id resolved → one narration line, nothing filed.
func File(findingsDir, teamID, relatedIdentifier string, filer Filer, searcher Searcher, log EventSink) {
	text, err := os.ReadFile(filepath.Join(findingsDir, dropboxFile))
	if err != nil {
		return // no dropbox file → nothing to file (the common, friction-free case)
	}

	parsed := findings.Parse(string(text))
	if parsed.Error != "" {
		log.Event("findings ✗ dropbox unreadable: " + parsed.Error)
		return
	}
	if len(parsed.Findings) == 0 {
		return
	}
	if teamID == "" {
		log.Event(fmt.Sprintf(
			"findings ✗ %d dropped but no team id resolved for %s",
			len(parsed.Findings), relatedIdentifier,
		))
		return
	}

	// Look up findings already tracked so we don't re-file duplicates across runs.
	// Best-effort (ADR-0001): a search failure degrades to filing everything, the
	// pre-dedup behaviour — never a crash.
	tracked := openTracked(teamID, searcher, log)

	for _, f := range parsed.Findings {
		if existing, ok := tracked[matchKey(f.Key, f.Title)]; ok {
			log.Event(fmt.Sprintf("finding ↩ already tracked: %s — %s", existing.Identifier, existing.Title))
			continue
		}
		created, err := filer.FileFinding(f, linear.FileFindingOptions{
			TeamID: teamID, RelatedIdentifier: relatedIdentifier,
		})
		if err != nil {
			log.Event(fmt.Sprintf("finding ✗ failed to file %q: %s", f.Title, err.Error()))
			continue
		}
		// Register what we just filed so a later finding in the same dropbox that
		// shares its key dedups against it too, not just across runs.
		tracked[matchKey(f.Key, f.Title)] = linear.ExistingFinding{Identifier: created.Identifier, Title: f.Title, Key: f.Key}
		log.Event(fmt.Sprintf("finding filed: %s — %s", created.Identifier, f.Title))
	}
}

// openTracked returns the already-filed harness findings keyed by match key,
// limited to OPEN issues — a closed/canceled match must not suppress a re-file.
// A search failure is narrated once and degrades to an empty map (file all).
func openTracked(teamID string, searcher Searcher, log EventSink) map[string]linear.ExistingFinding {
	tracked := map[string]linear.ExistingFinding{}
	existing, err := searcher.SearchFindings(teamID)
	if err != nil {
		log.Event("findings ⚠ dedup search failed, filing without dedup: " + err.Error())
		return tracked
	}
	for _, e := range existing {
		if e.Closed {
			continue
		}
		tracked[matchKey(e.Key, e.Title)] = e
	}
	return tracked
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
// ClearDropbox wipes that dropbox. Best-effort (ADR-0001): a Linear search
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

	// Any classes the prior run dropped but that may never have reached Linear
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
