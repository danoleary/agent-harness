// Package sentry fetches, on the host, the Sentry issues a ticket body cites, so
// the harness can put them into the Stage prompt the way it puts the ticket body
// there (ADR-0041). The sandbox reaches only Anthropic (ADR-0002) and the Sentry
// token never enters it.
//
// Event data carries user PII. Fetch never decodes the event's user object or its
// raw tags, and Gather — the only way an issue reaches the prompt — redacts the
// `User` section, `user.*` lines, emails and IP addresses from every field it
// keeps before cutting the event to its budget.
package sentry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultBaseURL is the Sentry SaaS site, the base_url a `[sentry]` table that
// names none gets.
const DefaultBaseURL = "https://sentry.io"

// MaxIssues caps how many cited issues the harness fetches for one ticket. A
// ticket that cites more names the rest as not fetched.
const MaxIssues = 5

// EventBudget is the most bytes of one formatted event that enter the prompt.
const EventBudget = 8 << 10

// Timeout bounds each Sentry request, so a slow Sentry delays dispatch by seconds
// at most and never blocks it.
const Timeout = 10 * time.Second

// Ref is one issue a ticket cites: by short ID (`HERD-3P`) or by the numeric
// group ID in an issue URL. Exactly one is set.
type Ref struct {
	ShortID string
	GroupID string
}

// String names the ref as the ticket did, for the log and the envelope.
func (r Ref) String() string {
	if r.ShortID != "" {
		return r.ShortID
	}
	return "issue " + r.GroupID
}

// Detect returns the issues body cites, in the order it first cites them: short
// IDs whose prefix is one of projects, and issue URLs under baseURL or the org's
// `<org>.sentry.io` subdomain. The project list is what keeps a tracker Key such
// as `BEH-1148` from reading as a Sentry issue.
func Detect(body, org, baseURL string, projects []string) []Ref {
	type hit struct {
		at  int
		ref Ref
	}
	var hits []hit
	if len(projects) > 0 {
		quoted := make([]string, len(projects))
		for i, p := range projects {
			quoted[i] = regexp.QuoteMeta(p)
		}
		re := regexp.MustCompile(`\b(?:` + strings.Join(quoted, "|") + `)-[0-9A-Z]+\b`)
		for _, m := range re.FindAllStringIndex(body, -1) {
			hits = append(hits, hit{m[0], Ref{ShortID: body[m[0]:m[1]]}})
		}
	}
	for _, re := range issueURLPatterns(org, baseURL) {
		for _, m := range re.FindAllStringSubmatchIndex(body, -1) {
			hits = append(hits, hit{m[0], Ref{GroupID: body[m[2]:m[3]]}})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	var refs []Ref
	seen := map[Ref]bool{}
	for _, h := range hits {
		if !seen[h.ref] {
			seen[h.ref] = true
			refs = append(refs, h.ref)
		}
	}
	return refs
}

// issueURLPatterns match an issue URL on the configured site and, for a site with
// org subdomains, on `<org>.<host>`. The group ID is the first submatch.
func issueURLPatterns(org, baseURL string) []*regexp.Regexp {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" {
		return nil
	}
	const tail = `(?:/[\w.~%-]+)*/issues/([0-9]+)\b`
	prefix := regexp.QuoteMeta(u.Host + u.Path)
	pats := []*regexp.Regexp{regexp.MustCompile(`https?://` + prefix + tail)}
	if org != "" {
		pats = append(pats, regexp.MustCompile(`https?://`+regexp.QuoteMeta(org)+`\.`+prefix+tail))
	}
	return pats
}

// Issue is one fetched, scrubbed Sentry issue and its recommended event.
type Issue struct {
	ID        string `json:"id"`
	ShortID   string `json:"shortId"`
	Title     string `json:"title"`
	Culprit   string `json:"culprit"`
	Permalink string `json:"permalink"`
	Level     string `json:"level"`
	Status    string `json:"status"`
	// Count is Sentry's event count, which its API sends as a string.
	Count     string `json:"count"`
	UserCount int    `json:"userCount"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	// Event is the recommended event's formatted markdown, scrubbed and cut to
	// EventBudget, or a line saying why it could not be fetched.
	Event string `json:"-"`
}

// Missing is a cited issue the harness could not put into the prompt, and why.
type Missing struct {
	Ref    string
	Reason string
}

// Snapshot is what the harness fetched for one ticket at dispatch time.
type Snapshot struct {
	Issues  []Issue
	Missing []Missing
}

// Gather fetches the first MaxIssues refs and records every other one, and every
// failed fetch, as Missing. An issue fetched twice — by short ID and by URL — is
// kept once. Every kept issue is scrubbed of user data, whatever fetch returned,
// and its event is then cut to EventBudget.
func Gather(refs []Ref, fetch func(Ref) (Issue, error)) Snapshot {
	var s Snapshot
	seen := map[string]bool{}
	for i, r := range refs {
		if i >= MaxIssues {
			s.Missing = append(s.Missing, Missing{Ref: r.String(), Reason: fmt.Sprintf("over the %d-issue cap per ticket", MaxIssues)})
			continue
		}
		issue, err := fetch(r)
		if err != nil {
			s.Missing = append(s.Missing, Missing{Ref: r.String(), Reason: err.Error()})
			continue
		}
		if issue.ID != "" && seen[issue.ID] {
			continue
		}
		seen[issue.ID] = true
		s.Issues = append(s.Issues, clean(issue))
	}
	return s
}

// Client reads one Sentry org's issues with a host-held auth token.
type Client struct {
	BaseURL string
	Org     string
	Token   string
	HTTP    *http.Client
}

// ErrNoToken is Fetch's and Ping's error when the host has no token.
var ErrNoToken = errors.New("SENTRY_AUTH_TOKEN is not set on the host")

// ErrHTTP wraps every error Sentry answered with a non-2xx status, so a caller
// can tell a refusal from a network failure.
var ErrHTTP = errors.New("Sentry refused the request")

// Fetch reads the issue r names and its recommended event's formatted markdown,
// unscrubbed: hand it to Gather, which scrubs it. A failed event fetch keeps the
// issue, with the reason in place of the event: the issue's counts are still the
// measurement.
func (c Client) Fetch(r Ref) (Issue, error) {
	var issue Issue
	if r.ShortID != "" {
		var res struct {
			Group Issue `json:"group"`
		}
		if err := c.get("/shortids/"+url.PathEscape(r.ShortID)+"/", &res); err != nil {
			return Issue{}, err
		}
		issue = res.Group
	} else if err := c.get("/issues/"+url.PathEscape(r.GroupID)+"/", &issue); err != nil {
		return Issue{}, err
	}
	if issue.ID == "" {
		issue.ID = r.GroupID
	}

	var event struct {
		// Only the formatted body is decoded: the user object and the raw tags never
		// leave this function.
		Formatted string `json:"formatted"`
	}
	if err := c.get("/issues/"+url.PathEscape(issue.ID)+"/events/recommended/?llmFormat=markdown&collapse=adjacentEvents", &event); err != nil {
		issue.Event = "(the recommended event could not be fetched: " + err.Error() + ")"
		return issue, nil
	}
	issue.Event = event.Formatted
	return issue, nil
}

// clean scrubs every free-text field of an issue, then cuts its event to
// EventBudget. Scrubbing first means a cut never splits an email or an address
// into a fragment the redaction no longer matches.
func clean(i Issue) Issue {
	for _, f := range []*string{&i.Title, &i.Culprit, &i.Permalink, &i.Event} {
		*f = Scrub(*f)
	}
	i.Event = truncate(i.Event, EventBudget)
	return i
}

// Ping makes the one cheap authenticated call doctor checks the token with: the
// org itself, which needs only `org:read`.
func (c Client) Ping() error {
	var org struct{}
	return c.get("/", &org)
}

// get reads one org-scoped API path into v.
func (c Client) get(path string, v any) error {
	if c.Token == "" {
		return ErrNoToken
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: Timeout}
	}
	u := strings.TrimRight(c.BaseURL, "/") + "/api/0/organizations/" + url.PathEscape(c.Org) + path
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return &httpError{status: res.StatusCode, path: req.URL.Path}
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(v); err != nil {
		return fmt.Errorf("Sentry sent an unreadable response for %s: %w", req.URL.Path, err)
	}
	return nil
}

// httpError is a non-2xx answer; it reads as the status and the path asked for.
type httpError struct {
	status int
	path   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("Sentry answered HTTP %d for %s", e.status, e.path)
}
func (e *httpError) Is(target error) bool { return target == ErrHTTP }

var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	ipv4RE  = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	// ipv6RE matches a full address (at least four groups, so a 10:20:30 clock time
	// does not) or a `::`-compressed one.
	ipv6RE = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){3,7}[0-9a-f]{1,4}\b|\b(?:[0-9a-f]{1,4}:){1,6}:(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?|::(?:ffff:)?[0-9a-f]{1,4}(?:[:.][0-9a-f]{1,4})+`)
	// headingRE is a markdown heading; its first group is the hashes.
	headingRE = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	// userLineRE is a `user` or `user.*` key-value line, as Sentry's markdown
	// renders tags and context.
	userLineRE = regexp.MustCompile(`(?i)^\s*(?:[-*]\s*)?(?:\*\*|` + "`" + `)?user(?:\.[\w.]+)?(?:\*\*|` + "`" + `)?\s*[:=]`)
)

// Scrub removes user PII from Sentry text: a `User` section of formatted
// markdown, every `user`/`user.*` line, and every email and IP address.
func Scrub(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	skipLevel := 0
	for _, l := range lines {
		if m := headingRE.FindStringSubmatch(l); m != nil {
			level := len(m[1])
			if skipLevel > 0 && level > skipLevel {
				continue
			}
			skipLevel = 0
			if strings.EqualFold(strings.TrimSpace(m[2]), "user") {
				skipLevel = level
				continue
			}
		} else if skipLevel > 0 {
			continue
		}
		if userLineRE.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	out := strings.Join(kept, "\n")
	out = emailRE.ReplaceAllString(out, "[email removed]")
	out = ipv6RE.ReplaceAllString(out, "[ip removed]")
	out = ipv4RE.ReplaceAllString(out, "[ip removed]")
	return out
}

// truncate cuts s to at most n bytes on a rune boundary, saying so.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n… (truncated by the harness at %d bytes)", n)
}
