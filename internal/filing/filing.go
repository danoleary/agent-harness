// Package filing files the harness-improvement findings a session dropped into
// its `/findings/out.json` dropbox to Linear. It is the host-side step that runs
// after a session returns (ADR-0001: the harness owns all Linear I/O); the
// agent only writes the dropbox, never reaches Linear itself.
package filing

import (
	"fmt"
	"os"
	"path/filepath"

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
func File(findingsDir, teamID, relatedIdentifier string, filer Filer, log EventSink) {
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

	for _, f := range parsed.Findings {
		created, err := filer.FileFinding(f, linear.FileFindingOptions{
			TeamID: teamID, RelatedIdentifier: relatedIdentifier,
		})
		if err != nil {
			log.Event(fmt.Sprintf("finding ✗ failed to file %q: %s", f.Title, err.Error()))
			continue
		}
		log.Event(fmt.Sprintf("finding filed: %s — %s", created.Identifier, f.Title))
	}
}
