package filing

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danoleary/agent-harness/internal/findings"
)

// ciFlakeKeyPrefix starts the key the retrospective gives a CI-flake finding,
// `ci-flaky-check-<check>`, so a check that flakes on every PR dedupes into one
// ticket (#357).
const ciFlakeKeyPrefix = "ci-flaky-check-"

// ciFlakeExcerptLines caps how much of the recorded failed log a filed body
// carries: the failure line sits at the tail, and the body is a ticket, not a
// log viewer (#378).
const ciFlakeExcerptLines = 40

// flakeSection is one run's failed attempt in a ci-flake-<run-id>.log: the
// failing step of each check, and the record's lines.
type flakeSection struct {
	file  string
	steps map[string]string // check name → failing step
	lines []string
}

// withCIFlakeEvidence copies the recorded failed attempt into the body of each
// ci-flaky-check finding: the check's failing step and the tail of its failed
// log, from the newest ci-flake-*.log in logDir that names the check. The CI
// watch writes that record host-side (#371), and the session that picks the
// ticket up runs in a sandbox with no gh and no network (ADR-0002), so the body
// is the only way the evidence reaches it (#378). With no record, the body says
// the step is unknown and the log cannot be fetched, so the session does not
// plan around a step it cannot take. Every other finding is returned as is.
func withCIFlakeEvidence(fs []findings.Finding, logDir string) []findings.Finding {
	var sections []flakeSection
	read := false
	out := make([]findings.Finding, len(fs))
	for i, f := range fs {
		out[i] = f
		key := strings.ToLower(strings.TrimSpace(f.Key))
		if !strings.HasPrefix(key, ciFlakeKeyPrefix) {
			continue
		}
		if !read {
			sections, read = readFlakeRecords(logDir), true
		}
		out[i].Body = strings.TrimRight(f.Body, "\n") + "\n\n" + flakeEvidence(sections, strings.TrimPrefix(key, ciFlakeKeyPrefix))
	}
	return out
}

// flakeEvidence is the section a ci-flaky-check body gains for the check its key
// slug names.
func flakeEvidence(sections []flakeSection, slug string) string {
	s, check, ok := findFlake(sections, slug)
	if !ok {
		return "### Recorded CI flake log\n\n" +
			"The harness found no record of this check's failed attempt (`ci-flake-*.log`) when it filed this, so the failing step is unknown. " +
			"The log cannot be fetched from the sandbox (no `gh`, no network): do not plan around fetching it."
	}
	excerpt := jobLines(s.lines, check)
	if len(excerpt) > ciFlakeExcerptLines {
		excerpt = excerpt[len(excerpt)-ciFlakeExcerptLines:]
	}
	text := strings.Join(excerpt, "\n")
	fence := codeFence(text)
	return fmt.Sprintf("### Recorded CI flake log\n\n"+
		"From `%s`, which the harness kept host-side from the failed attempt; the sandbox cannot fetch it (no `gh`, no network), so this is the evidence there is.\n\n"+
		"- Check: `%s`\n- Failing step: `%s`\n\nLast %d lines of its failed log:\n\n%s\n%s\n%s",
		s.file, check, s.steps[check], len(excerpt), fence, text, fence)
}

// findFlake picks the newest section that records a check whose name slugs to
// slug, else the newest whose check slug and slug contain one another (a key
// slugged a little differently from the check name).
func findFlake(sections []flakeSection, slug string) (flakeSection, string, bool) {
	for _, exact := range []bool{true, false} {
		for i := len(sections) - 1; i >= 0; i-- {
			s := sections[i]
			for _, check := range sortedChecks(s.steps) {
				c := slugify(check)
				if c == slug || (!exact && c != "" && (strings.Contains(slug, c) || strings.Contains(c, slug))) {
					return s, check, true
				}
			}
		}
	}
	return flakeSection{}, "", false
}

func sortedChecks(steps map[string]string) []string {
	checks := make([]string, 0, len(steps))
	for c := range steps {
		checks = append(checks, c)
	}
	sort.Strings(checks)
	return checks
}

// jobLines is the check's job block in a section (gh names the job's lines by
// the check), else every line after the section's step lines.
func jobLines(lines []string, check string) []string {
	header := "----- " + check + ":"
	var job, rest []string
	in := false
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "----- "):
			in = strings.HasPrefix(l, header)
		case strings.HasPrefix(l, "check ") && strings.Contains(l, " — failing step: "):
			continue
		case in:
			job = append(job, l)
		}
		if !strings.HasPrefix(l, "----- ") {
			rest = append(rest, l)
		}
	}
	if len(job) > 0 {
		return job
	}
	return rest
}

// readFlakeRecords reads every ci-flake-*.log in logDir into its run sections,
// oldest file first. Best-effort: an absent dir or unreadable file is skipped.
func readFlakeRecords(logDir string) []flakeSection {
	if logDir == "" {
		return nil
	}
	paths, _ := filepath.Glob(filepath.Join(logDir, "ci-flake-*.log"))
	type dated struct {
		path string
		mod  int64
	}
	var files []dated
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			files = append(files, dated{p, info.ModTime().UnixNano()})
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		if files[i].mod != files[j].mod {
			return files[i].mod < files[j].mod
		}
		return files[i].path < files[j].path
	})
	var sections []flakeSection
	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		sections = append(sections, parseFlakeRecord(filepath.Base(f.path), string(raw))...)
	}
	return sections
}

// parseFlakeRecord splits a record into the run sections the flake washer
// writes, each opened by a "===== run " header.
func parseFlakeRecord(file, raw string) []flakeSection {
	var sections []flakeSection
	for _, l := range strings.Split(raw, "\n") {
		if strings.HasPrefix(l, "===== run ") {
			sections = append(sections, flakeSection{file: file, steps: map[string]string{}})
			continue
		}
		if len(sections) == 0 || strings.TrimSpace(l) == "" {
			continue
		}
		s := &sections[len(sections)-1]
		if check, step, ok := strings.Cut(strings.TrimPrefix(l, "check "), " — failing step: "); ok && strings.HasPrefix(l, "check ") {
			s.steps[check] = step
		}
		s.lines = append(s.lines, l)
	}
	return sections
}

// codeFence is a backtick fence longer than any backtick run in text, so a log
// line cannot close it early.
func codeFence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}
