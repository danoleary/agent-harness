// Package overlap predicts which files a ticket will change and finds the work in
// flight that already changes them (#97). Two tickets with no dependency between
// them still conflict when they edit the same file in parallel, and only a human
// could untangle the second PR after the first merged. Selection defers such a
// ticket until the overlapping work merges or closes (ADR-0032).
//
// The prediction is cheap and literal: the paths, file names and backticked module
// names the ticket's title and body mention. A ticket that names no file in flight
// dispatches as it always did. A mention that names no file in particular — a
// directory, a generic name such as `index.ts`, a name inside a fenced code block
// — names nothing, so it cannot defer a ticket on its own (#281).
package overlap

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// Work is one unit of work in flight: an open harness PR or a claimed ticket's
// branch, and the files it changes against base.
type Work struct {
	// Label names the work in the deferral line: "PR #81", "#80 (claimed)".
	Label string
	// Branch is the work's head branch, so a ticket never defers behind its own.
	Branch string
	Files  []string
}

// Policy is the Consumer's setting for the check.
type Policy struct {
	// Threshold is how many of one Work's files a ticket must name to be deferred
	// behind it. 0 turns the check off.
	Threshold int
	// EscapeLabel, on a ticket, dispatches it whatever it overlaps: a human has
	// judged the two safe to run in parallel.
	EscapeLabel string
}

// Deferral is why a ticket waits: the work it overlaps and the shared files.
type Deferral struct {
	Work  Work
	Files []string
}

// maxReasonFiles bounds the files a deferral line names, so a ticket that names
// many files does not flood the loop log.
const maxReasonFiles = 5

// Reason is the deferral's loop-log line: `deferred: overlaps PR #N on <files>`.
func (d Deferral) Reason() string {
	files := d.Files
	more := ""
	if len(files) > maxReasonFiles {
		more = fmt.Sprintf(" and %d more", len(files)-maxReasonFiles)
		files = files[:maxReasonFiles]
	}
	return fmt.Sprintf("deferred: overlaps %s on %s%s", d.Work.Label, strings.Join(files, ", "), more)
}

// Check decides whether t waits for the work in flight. branch is t's own branch,
// which is never work it waits on. The first Work that shares at least Threshold
// of the files t names is the deferral; none, an off Threshold, or the escape label
// on t dispatches it.
func Check(t ticket.Ticket, branch string, work []Work, p Policy) (Deferral, bool) {
	if p.Threshold <= 0 || t.HasLabel(p.EscapeLabel) {
		return Deferral{}, false
	}
	predicted := Predict(text(t))
	if len(predicted) == 0 {
		return Deferral{}, false
	}
	for _, w := range work {
		if w.Branch != "" && w.Branch == branch {
			continue
		}
		if shared := Match(predicted, w.Files); len(shared) >= p.Threshold {
			return Deferral{Work: w, Files: shared}, true
		}
	}
	return Deferral{}, false
}

// Named is the files out of files that t's title and body name: a claimed
// ticket's work before it has changed anything, resolved against the repo's files.
func Named(t ticket.Ticket, files []string) []string {
	return Match(Predict(text(t)), files)
}

// text is the part of a ticket a prediction reads.
func text(t ticket.Ticket) string { return t.Title + "\n" + t.Description }

// mentionKind is how a mention names files.
type mentionKind int

const (
	// pathMention contains a slash: it names a file or a path's tail. A directory
	// is context, not a plan to edit every file under it, so it names none (#281).
	pathMention mentionKind = iota
	// fileMention is a bare file name: it names every file with its stem, so
	// `controls.tsx` names `controls.test.tsx` and `controls.stories.tsx` too.
	fileMention
	// moduleMention is a backticked bare name: the stem of a file or the name of the
	// directory it sits in (a Go package, a TS module).
	moduleMention
)

// Mention is one way a ticket names files.
type Mention struct {
	kind mentionKind
	text string
}

// separators split prose into candidate tokens: whitespace and the Markdown and
// table punctuation a path sits between.
func separator(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '|', '(', ')', '[', ']', '<', '>', '{', '}', '"', '\'', '`', ',', ';', '*':
		return true
	}
	return false
}

// extRe is a file extension: short, lowercase, at least one letter. It keeps a Go
// selector (`loop.Run`) or a version (`v1.2`) from reading as a file name.
var extRe = regexp.MustCompile(`^[a-z0-9]{1,5}$`)

// backtickRe is an inline code span without whitespace: the one place a bare word
// is a module name rather than prose.
var backtickRe = regexp.MustCompile("`([^`\\s]+)`")

// moduleRe is a name a module can have.
var moduleRe = regexp.MustCompile(`^[A-Za-z0-9_.-]*[A-Za-z][A-Za-z0-9_.-]*$`)

// genericStems are file and module names so common that a bare mention of one
// names no file in particular: `index.ts` sits in every directory of a web app,
// and deferred a ticket behind any PR that touched one (#281). Name such a file by
// its path.
var genericStems = map[string]bool{
	"index": true, "main": true, "route": true, "routes": true, "config": true,
	"types": true, "env": true, "utils": true, "util": true, "helpers": true,
	"constants": true, "page": true, "layout": true,
}

// distinctive reports whether a bare file or module name names files in
// particular. A dot-prefixed name (`.figma.tsx`, `.env.example`) has an empty
// stem, which every dotfile shares, so it names none (#281).
func distinctive(name string) bool {
	s := stem(name)
	return s != "" && !genericStems[strings.ToLower(s)]
}

// fenceRe opens or closes a fenced code block.
var fenceRe = regexp.MustCompile("^ {0,3}(```|~~~)")

// prose is text without its fenced code blocks: a log excerpt or a stack trace
// quotes the files a failure passed through as evidence, not as a plan to edit
// them (#281). An unclosed fence runs to the end, as Markdown renders it.
func prose(text string) string {
	var b strings.Builder
	fenced := false
	for _, line := range strings.Split(text, "\n") {
		if fenceRe.MatchString(line) {
			fenced = !fenced
			continue
		}
		if !fenced {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// Predict reads the files a ticket's text names, in first-seen order.
func Predict(text string) []Mention {
	text = prose(text)
	var out []Mention
	seen := map[Mention]bool{}
	add := func(m Mention) {
		if m.kind != pathMention && !distinctive(m.text) {
			return
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, tok := range strings.FieldsFunc(text, separator) {
		tok, ok := clean(tok)
		if !ok {
			continue
		}
		switch {
		case strings.Contains(tok, "/"):
			add(Mention{pathMention, tok})
		case hasExt(tok):
			add(Mention{fileMention, tok})
		}
	}
	for _, m := range backtickRe.FindAllStringSubmatch(text, -1) {
		tok, ok := clean(m[1])
		if ok && !strings.Contains(tok, "/") && !hasExt(tok) && moduleRe.MatchString(tok) {
			add(Mention{moduleMention, tok})
		}
	}
	return out
}

// clean trims a token to the path it names, or reports it names none: a URL, a
// host-qualified path (`github.com/o/r`), a `:line` suffix and sentence
// punctuation are not part of a repo path.
func clean(tok string) (string, bool) {
	if strings.Contains(tok, "://") {
		return "", false
	}
	if i := strings.IndexByte(tok, ':'); i >= 0 {
		tok = tok[:i]
	}
	tok = strings.TrimRight(tok, ".!?/")
	tok = strings.TrimPrefix(tok, "./")
	tok = strings.TrimLeft(tok, "/")
	if tok == "" {
		return "", false
	}
	if first, _, found := strings.Cut(tok, "/"); found && strings.LastIndexByte(first, '.') > 0 {
		return "", false
	}
	return tok, true
}

// hasExt reports whether a slash-free token reads as a file name.
func hasExt(tok string) bool {
	i := strings.LastIndexByte(tok, '.')
	if i <= 0 {
		return false
	}
	ext := tok[i+1:]
	return extRe.MatchString(ext) && strings.ContainsAny(ext, "abcdefghijklmnopqrstuvwxyz")
}

// stem is a file name up to its first dot: `controls.test.tsx` → `controls`.
func stem(name string) string {
	s, _, _ := strings.Cut(name, ".")
	return s
}

// names reports whether m names file f.
func (m Mention) names(f string) bool {
	switch m.kind {
	case pathMention:
		return f == m.text || strings.HasSuffix(f, "/"+m.text)
	case fileMention:
		return stem(path.Base(f)) == stem(m.text)
	default:
		return stem(path.Base(f)) == m.text || path.Base(path.Dir(f)) == m.text
	}
}

// Match returns the files any mention names, sorted and de-duplicated.
func Match(mentions []Mention, files []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f] {
			continue
		}
		for _, m := range mentions {
			if m.names(f) {
				seen[f] = true
				out = append(out, f)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
