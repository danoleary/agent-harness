package filing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/findings"
)

// harnessFindingRecord is the on-disk shape of a harness-audience finding: the
// finding itself plus the ticket whose session surfaced it (provenance BEH-640's
// upstreaming will attribute the report to). It's a superset of the dropbox
// finding so a local artifact carries everything a later upstream file needs.
type harnessFindingRecord struct {
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Audience findings.Audience `json:"audience"`
	Kind     string            `json:"kind,omitempty"`
	Key      string            `json:"key,omitempty"`
	// RelatedKey is the ticket that surfaced the finding.
	RelatedKey string `json:"related_key,omitempty"`
}

// HarnessFindingsDir is the local artifact dir where harness-audience findings
// are written by default (ADR-0011): `<checkout>/.agent-harness/harness-findings`.
// It sits under the Consumer checkout so nothing leaves the repo without the
// explicit opt-in that lands in BEH-640. Single-sourced here so the stages that
// write it and any future upstreamer read the same path.
func HarnessFindingsDir(checkoutPath string) string {
	return filepath.Join(checkoutPath, ".agent-harness", "harness-findings")
}

// WriteHarnessFindings writes each harness-audience finding to harnessDir as a
// single JSON file per finding, named by a stable slug (its key, else a
// slugified title) so a re-run of the same class overwrites rather than
// accumulates duplicates (ADR-0011). This is the DEFAULT sink for harness
// findings: a local artifact in the Consumer repo the owner reviews — nothing
// leaves the repo without the explicit opt-in that lands in BEH-640.
// Best-effort (ADR-0001): a mkdir/write failure degrades to a narration line,
// never a crash. An empty slice is a no-op (the dir is not created).
func WriteHarnessFindings(harnessDir, relatedIdentifier string, fs []findings.Finding, log EventSink) {
	if len(fs) == 0 {
		return
	}
	if err := os.MkdirAll(harnessDir, 0o755); err != nil {
		log.Event("harness findings ✗ could not create " + harnessDir + ": " + err.Error())
		return
	}
	for _, f := range fs {
		name := harnessFindingSlug(f) + ".json"
		rec := harnessFindingRecord{
			Title: f.Title, Body: f.Body, Audience: findings.AudienceHarness,
			Kind: f.Kind, Key: f.Key, RelatedKey: relatedIdentifier,
		}
		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			log.Event(fmt.Sprintf("harness finding ✗ could not encode %q: %s", f.Title, err.Error()))
			continue
		}
		path := filepath.Join(harnessDir, name)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			log.Event(fmt.Sprintf("harness finding ✗ could not write %q: %s", f.Title, err.Error()))
			continue
		}
		log.Event(fmt.Sprintf("harness finding written locally: %s — %s", name, f.Title))
	}
}

// harnessFindingSlug is the stable per-class filename stem: the explicit key when
// set (already a lowercase failure-class slug by convention), else a slug of the
// title. Stable so a re-run of the same class overwrites its own file rather than
// piling up near-duplicates.
func harnessFindingSlug(f findings.Finding) string {
	if k := slugify(f.Key); k != "" {
		return k
	}
	if s := slugify(f.Title); s != "" {
		return s
	}
	return "finding"
}

// slugify lowercases s and replaces every run of non-alphanumeric characters with
// a single hyphen, trimming leading/trailing hyphens — a filesystem-safe stem.
func slugify(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen && b.Len() > 0 {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}
