// Package filing files the harness-improvement findings a session dropped into
// its `/findings/out.json` dropbox, through [tracker.FindingsSink] — layer 3 of
// the Tracker port, which every adapter already implements. It is the host-side
// step that runs after a session returns (ADR-0010, retaining ADR-0001's
// invariant: the harness owns all tracker I/O); the agent only writes the
// dropbox, never reaches the tracker itself.
//
// [Router] is the entry point: it holds the collaborators — the two sinks, the
// semantic matcher, the local artifact dir, the narration log — and its methods
// take only the per-session data. The sinks were three ad-hoc single-method
// interfaces declared here (Filer, Searcher, OccurrenceRecorder) that restated,
// one method each, what tracker.FindingsSink already names as a trio.
package filing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// dropboxFile is the fixed filename the agent drops findings into, under the
// session's findings dir (mounted at /findings in the sandbox).
const dropboxFile = "out.json"

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

// Router is the host-side findings step, holding the collaborators one run
// files through: the Consumer's tracker, the optional upstream sink, the
// best-effort semantic matcher, the local artifact dir, and the narration log.
// Its methods take only the per-session data (which dropbox, which container,
// which ticket), so adding a collaborator costs a field rather than another
// positional parameter at every call site.
type Router struct {
	// Project is the Consumer's tracker — the sink for project-audience findings,
	// and for every finding on the audience-blind [Router.File] path. Required.
	Project tracker.FindingsSink
	// Upstream is the opt-in public-harness-repo sink for harness-audience
	// findings (ADR-0011). Nil — the default — writes them to HarnessDir instead,
	// so nothing leaves the repo without the explicit opt-in.
	Upstream *Upstream
	// Matcher is the best-effort semantic dedup pass. Nil skips it, degrading to
	// exact key/title dedup (the pre-BEH-573 behaviour).
	Matcher SemanticMatcher
	// HarnessDir is the local artifact dir harness findings land in when Upstream
	// is nil. See [HarnessFindingsDir].
	HarnessDir string
	// Log receives the narration. Required: every failure degrades to a line here.
	Log EventSink
}

// File reads the findings dropbox in findingsDir and files each finding to the
// Project sink, referencing relatedIdentifier (the worked ticket). It never
// returns an error: every failure is degraded to a narration line, so one bad
// dropbox or a transient tracker failure can't crash the run (ADR-0001:
// degraded, not broken).
//   - No dropbox file → nothing to file (the common, friction-free case).
//   - Dropbox present but unreadable/malformed → one narration line, nothing filed.
//   - Findings present but no team id resolved → one narration line, nothing filed.
//
// Dedup runs in two passes before filing: the fast exact key/title match
// (matchKey), then — for anything that misses — a best-effort semantic pass via
// Matcher. A match (either pass) is recorded as a recurrence on the existing
// issue (comment-and-bump) instead of filing a duplicate.
//
// File is the audience-blind primitive: it files everything to the tracker. The
// implementation stage uses it because its dropbox protocol asks for harness
// friction only and never classifies; [Router.Route] is the audience-aware form.
func (r Router) File(findingsDir, teamID, relatedIdentifier string) {
	parsed, ok := r.readDropbox(findingsDir)
	if !ok {
		return
	}
	r.fileTo(r.Project, parsed.Findings, teamID, relatedIdentifier, r.Matcher)
}

// Upstream is the opt-in public-harness-repo sink for harness-audience findings
// (ADR-0011/BEH-640). When wired, Route files harness findings to it — a tracker
// adapter bound to the public repo, using the host's GH_TOKEN — instead of the
// local artifact dir. Sink is the same [tracker.FindingsSink] the Consumer's own
// tracker satisfies, so the shared dedup pipeline files them with cross-project
// key dedup and project-tagged recurrences.
type Upstream struct {
	// Sink is the public repo's finding sink (a github adapter through the port).
	Sink tracker.FindingsSink
	// Container is the upstream repo slug ("owner/name"), passed as the sink's
	// teamID. The github adapter is repo-bound and ignores it, but it must be
	// non-empty for the team-id guard in fileTo to proceed.
	Container string
	// Project is the reporting-project name stamped on every upstream filing and
	// recurrence comment, so the public repo shows which project surfaced a finding
	// (ADR-0011 provenance). Empty falls back to the worked ticket identifier.
	Project string
}

// Route reads the findings dropbox once and routes each finding by its audience
// (ADR-0011): project findings file to the Consumer's tracker through the same
// dedup pipeline [Router.File] uses. Harness findings go to the local artifact
// dir (HarnessDir) by default, or — when Upstream is wired (feedback.upstream =
// github) — to the public harness repo with cross-project key dedup and
// project-tagged recurrences. It is the audience-aware successor to File the
// stages call; File remains the plain file-everything-to-the-tracker primitive.
// Best-effort throughout (ADR-0001): every failure degrades to narration, never a
// crash.
func (r Router) Route(findingsDir, teamID, relatedIdentifier string) {
	parsed, ok := r.readDropbox(findingsDir)
	if !ok {
		return
	}
	project, harness := partitionByAudience(parsed.Findings)
	r.routeHarness(harness, relatedIdentifier)
	r.fileTo(r.Project, project, teamID, relatedIdentifier, r.Matcher)
}

// routeHarness sends the harness-audience findings to their configured sink: the
// public harness repo when Upstream is wired (project-tagged, deduped by key), or
// the local artifact dir otherwise (the default — nothing leaves the repo).
func (r Router) routeHarness(harness []findings.Finding, relatedIdentifier string) {
	if r.Upstream == nil {
		WriteHarnessFindings(r.HarnessDir, relatedIdentifier, harness, r.Log)
		return
	}
	// Stamp the reporting project as the "related" tag so upstream filings read
	// "Surfaced during <project>" and recurrences read "Recurred in <project>";
	// fall back to the worked ticket when no project name is configured. Dedup is
	// by key only (nil matcher): cross-project semantic dedup against a public repo
	// is out of scope — the AC is key-based dedup.
	tag := r.Upstream.Project
	if strings.TrimSpace(tag) == "" {
		tag = relatedIdentifier
	}
	r.fileTo(r.Upstream.Sink, harness, r.Upstream.Container, tag, nil)
}

// readDropbox reads and parses the dropbox in findingsDir. ok is false — with a
// narration only on a real parse error — when there is nothing to act on: an
// absent file (the common, friction-free case) or a malformed one.
func (r Router) readDropbox(findingsDir string) (findings.Parsed, bool) {
	text, err := os.ReadFile(filepath.Join(findingsDir, dropboxFile))
	if err != nil {
		return findings.Parsed{}, false // no dropbox file → nothing to file
	}
	parsed := findings.Parse(string(text))
	if parsed.Error != "" {
		r.Log.Event("findings ✗ dropbox unreadable: " + parsed.Error)
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

// fileTo files the given findings to one sink with cross-run dedup. It is the
// shared engine behind File (all findings, to Project), Route's project half
// (Project), and Route's harness half (the Upstream sink) — which is why the
// sink and the matcher are parameters rather than read off r: the upstream path
// passes its own sink and a nil matcher. An empty set is a no-op; a non-empty
// set with no team id resolved is one narration line and nothing filed.
func (r Router) fileTo(sink tracker.FindingsSink, fs []findings.Finding, teamID, relatedIdentifier string, matcher SemanticMatcher) {
	if len(fs) == 0 {
		return
	}
	if teamID == "" {
		r.Log.Event(fmt.Sprintf(
			"findings ✗ %d dropped but no team id resolved for %s",
			len(fs), relatedIdentifier,
		))
		return
	}

	// Look up findings already tracked so we don't re-file duplicates across runs.
	// Best-effort (ADR-0001): a search failure degrades to filing everything (an
	// empty tracked map AND no open set, so the semantic pass is also skipped) —
	// the pre-dedup behaviour, never a crash.
	tracked, open := r.openTracked(sink, teamID)

	for _, f := range fs {
		// 1. Exact match — the fast short-circuit, preserved ahead of the model.
		if existing, ok := tracked[matchKey(f.Key, f.Title)]; ok {
			r.recordRecurrence(sink, existing, relatedIdentifier)
			continue
		}
		// 2. Semantic match — best-effort; an error, a nil matcher, an empty open
		// set, or an unrecognised id all fall through to filing as new.
		if existing, ok := r.semanticMatch(f, open, matcher); ok {
			r.recordRecurrence(sink, existing, relatedIdentifier)
			// Register so a later same-class finding in this dropbox also dedups
			// against it (its exact key/title now points at the matched issue).
			tracked[matchKey(f.Key, f.Title)] = existing
			continue
		}
		// 3. No match — file a new issue.
		created, err := sink.FileFinding(f, tracker.FileFindingOptions{
			TeamID: teamID, RelatedKey: relatedIdentifier,
		})
		if err != nil {
			r.Log.Event(fmt.Sprintf("finding ✗ failed to file %q: %s", f.Title, err.Error()))
			continue
		}
		// Register what we just filed so a later finding in the same dropbox that
		// shares its key/title — or reads as the same class — dedups against it too,
		// not just across runs.
		filed := tracker.ExistingFinding{Identifier: created.Identifier, Title: f.Title, Key: f.Key}
		tracked[matchKey(f.Key, f.Title)] = filed
		open = append(open, filed)
		r.Log.Event(fmt.Sprintf("finding filed: %s — %s", created.Identifier, f.Title))
	}
}

// recordRecurrence handles a finding that matched an already-tracked issue: it
// appends an occurrence comment to the existing issue and bumps its occurrence
// count (BEH-573), turning N duplicate files into one issue carrying N
// occurrences — a better prioritisation signal than N near-identical issues.
// Best-effort (ADR-0001): a sink failure degrades to a skip narration, so a
// transient tracker error never files a duplicate or crashes.
func (r Router) recordRecurrence(sink tracker.FindingsSink, existing tracker.ExistingFinding, relatedIdentifier string) {
	count, err := sink.RecordOccurrence(existing.Identifier, relatedIdentifier)
	if err != nil {
		r.Log.Event(fmt.Sprintf("finding ⚠ recurrence not recorded on %s (skipped, not re-filed): %s", existing.Identifier, err.Error()))
		return
	}
	r.Log.Event(fmt.Sprintf("finding ↩ recurred (occurrence %d): %s — %s", count, existing.Identifier, existing.Title))
}

// semanticMatch asks the matcher whether f duplicates one of the open findings,
// resolving the returned identifier back to its ExistingFinding. Best-effort: a
// nil matcher, an empty open set, a model error, or an identifier not in the open
// set all yield ok=false so the finding is filed as new (the pre-semantic
// behaviour) rather than mis-deduped against an issue we can't verify.
func (r Router) semanticMatch(f findings.Finding, open []tracker.ExistingFinding, matcher SemanticMatcher) (tracker.ExistingFinding, bool) {
	if matcher == nil || len(open) == 0 {
		return tracker.ExistingFinding{}, false
	}
	id, err := matcher.MatchFinding(f, open)
	if err != nil {
		r.Log.Event("finding ⚠ semantic dedup failed, filing without it: " + err.Error())
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
	r.Log.Event("finding ⚠ semantic dedup returned unknown id " + id + ", filing as new")
	return tracker.ExistingFinding{}, false
}

// openTracked returns the already-filed harness findings both keyed by match key
// (for the exact short-circuit) and as an ordered slice (the candidate set for
// the semantic pass), limited to OPEN issues — a closed/canceled match must not
// suppress a re-file. A search failure is narrated once and degrades to an empty
// map and nil slice (file all, no semantic pass).
func (r Router) openTracked(sink tracker.FindingsSink, teamID string) (map[string]tracker.ExistingFinding, []tracker.ExistingFinding) {
	tracked := map[string]tracker.ExistingFinding{}
	existing, err := sink.SearchFindings(teamID)
	if err != nil {
		r.Log.Event("findings ⚠ dedup search failed, filing without dedup: " + err.Error())
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
// should treat as settled (BEH-539): the Project sink's OPEN filed findings, merged with
// any classes left in this ticket's prior dropbox. It MUST be called before
// ClearDropbox wipes that dropbox. Best-effort (ADR-0001): a tracker search
// failure degrades to the dropbox classes alone and narrates, never crashes. The
// result is sorted (key, then title) so the injected prompt is deterministic.
func (r Router) AlreadyFiled(findingsDir, teamID string) []PriorFinding {
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
	if existing, err := r.Project.SearchFindings(teamID); err != nil {
		r.Log.Event("retrospective context ⚠ already-filed lookup failed, prior-finding context limited to the dropbox: " + err.Error())
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
