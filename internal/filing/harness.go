package filing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// ProbableDuplicateOf is the slug of an already-written finding this one
	// closely matches by token overlap (#65) — the same class under a drifted key.
	// The owner reviewing the dir merges or deletes it; the harness never does.
	ProbableDuplicateOf string `json:"probable_duplicate_of,omitempty"`
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
// A finding whose key or title closely matches another class already in the dir
// (probablyDuplicate) is still written, but flagged with probable_duplicate_of
// and narrated, so key drift across tickets surfaces instead of silently piling
// up near-duplicate files (#65).
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
	written := readHarnessFindings(harnessDir)
	for _, f := range fs {
		slug := harnessFindingSlug(f)
		name := slug + ".json"
		rec := harnessFindingRecord{
			Title: f.Title, Body: f.Body, Audience: findings.AudienceHarness,
			Kind: f.Kind, Key: f.Key, RelatedKey: relatedIdentifier,
		}
		if dup, ok := probableDuplicateRecord(f, slug, written); ok {
			rec.ProbableDuplicateOf = dup
			log.Event(fmt.Sprintf("harness finding ⚠ %s is a probable duplicate of %s (close token overlap) — written anyway, flagged for review", name, dup))
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
		written[slug] = rec
	}
}

// readHarnessFindings returns every harness finding already in harnessDir, keyed
// by its file's slug. A local artifact has no closed state — it stays until the
// owner deletes it — so every record here counts as open. Best-effort: an absent
// dir, or a file that won't read or parse, is skipped.
func readHarnessFindings(harnessDir string) map[string]harnessFindingRecord {
	out := map[string]harnessFindingRecord{}
	entries, err := os.ReadDir(harnessDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(harnessDir, name))
		if err != nil {
			continue
		}
		var rec harnessFindingRecord
		if json.Unmarshal(data, &rec) != nil {
			continue
		}
		out[strings.TrimSuffix(name, ".json")] = rec
	}
	return out
}

// probableDuplicateRecord returns the slug of a written record f probably
// duplicates, skipping f's own slug (a re-run of the same class overwrites its
// file — that is the exact-key dedup, not drift). Slugs are checked in sorted
// order so the flag is deterministic.
func probableDuplicateRecord(f findings.Finding, slug string, written map[string]harnessFindingRecord) (string, bool) {
	slugs := make([]string, 0, len(written))
	for s := range written {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	for _, s := range slugs {
		if s == slug {
			continue
		}
		if rec := written[s]; probablyDuplicate(f, rec.Key, rec.Title) {
			return s, true
		}
	}
	return "", false
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
