package sentry

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		baseURL  string
		projects []string
		want     []Ref
	}{
		{
			name:     "a short id whose prefix is a configured project",
			body:     "Read the Sentry events for HERD-3P first.",
			projects: []string{"HERD"},
			want:     []Ref{{ShortID: "HERD-3P"}},
		},
		{
			// The ticket's own incident: BEH-1148 is the tracker Key, not a Sentry issue.
			name:     "a tracker key is not a short id",
			body:     "Seen in BEH-1148 and BEH-1179; read HERD-3P.",
			projects: []string{"HERD"},
			want:     []Ref{{ShortID: "HERD-3P"}},
		},
		{
			name:     "a prefix is matched whole, not as part of a longer word",
			body:     "SHERD-1 and HERDS-2 and HERD-",
			projects: []string{"HERD"},
			want:     nil,
		},
		{
			name:     "a short id is deduplicated",
			body:     "HERD-3P, again HERD-3P, and HERD-4",
			projects: []string{"HERD"},
			want:     []Ref{{ShortID: "HERD-3P"}, {ShortID: "HERD-4"}},
		},
		{
			name: "an issue URL under base_url, the org subdomain, and a short id, in body order",
			body: "See https://acme.sentry.io/issues/4504/?project=1 then HERD-3P then " +
				"https://sentry.io/organizations/acme/issues/77/ and https://acme.sentry.io/issues/4504/ again.",
			projects: []string{"HERD"},
			want:     []Ref{{GroupID: "4504"}, {ShortID: "HERD-3P"}, {GroupID: "77"}},
		},
		{
			name:     "a self-hosted base_url",
			body:     "https://sentry.acme.dev/organizations/acme/issues/12/ but not https://example.com/issues/13",
			baseURL:  "https://sentry.acme.dev/",
			projects: []string{"HERD"},
			want:     []Ref{{GroupID: "12"}},
		},
		{
			name:     "another org's subdomain is not this org's issue",
			body:     "https://other.sentry.io/issues/99/",
			projects: []string{"HERD"},
			want:     nil,
		},
		{
			name:     "several projects",
			body:     "API-1 and WEB-2Z",
			projects: []string{"WEB", "API"},
			want:     []Ref{{ShortID: "API-1"}, {ShortID: "WEB-2Z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.baseURL
			if base == "" {
				base = DefaultBaseURL
			}
			got := Detect(tc.body, "acme", base, tc.projects)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Detect = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRefString(t *testing.T) {
	if got := (Ref{ShortID: "HERD-3P"}).String(); got != "HERD-3P" {
		t.Errorf("short id ref = %q", got)
	}
	if got := (Ref{GroupID: "4504"}).String(); got != "issue 4504" {
		t.Errorf("group ref = %q", got)
	}
}

func TestGatherCapsAndRecordsFailures(t *testing.T) {
	refs := []Ref{{ShortID: "HERD-1"}, {ShortID: "HERD-2"}, {GroupID: "3"}, {ShortID: "HERD-4"}, {ShortID: "HERD-5"}, {ShortID: "HERD-6"}, {ShortID: "HERD-7"}}
	var fetched []string
	snap := Gather(refs, func(r Ref) (Issue, error) {
		fetched = append(fetched, r.String())
		switch r.ShortID {
		case "HERD-2":
			return Issue{}, errors.New("HTTP 404")
		case "HERD-4":
			// The same issue the URL named: rendered once.
			return Issue{ID: "3", ShortID: "HERD-4"}, nil
		}
		id := r.GroupID
		if id == "" {
			id = "g-" + r.ShortID
		}
		return Issue{ID: id, ShortID: r.ShortID}, nil
	})
	if len(fetched) != MaxIssues {
		t.Errorf("fetched %v, want the first %d only", fetched, MaxIssues)
	}
	var shorts []string
	for _, i := range snap.Issues {
		shorts = append(shorts, i.ID)
	}
	if want := []string{"g-HERD-1", "3", "g-HERD-5"}; !reflect.DeepEqual(shorts, want) {
		t.Errorf("issues = %v, want %v", shorts, want)
	}
	want := []Missing{
		{Ref: "HERD-2", Reason: "HTTP 404"},
		{Ref: "HERD-6", Reason: "over the 5-issue cap per ticket"},
		{Ref: "HERD-7", Reason: "over the 5-issue cap per ticket"},
	}
	if !reflect.DeepEqual(snap.Missing, want) {
		t.Errorf("missing = %+v, want %+v", snap.Missing, want)
	}
}

// fakeSentry serves the two endpoints Fetch reads, and asserts the bearer token.
func fakeSentry(t *testing.T, event string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	group := `{"id":"4504","shortId":"HERD-3P","title":"TypeError: x is undefined","culprit":"secrets.load","permalink":"https://acme.sentry.io/issues/4504/","level":"error","status":"unresolved","count":"128","userCount":17,"firstSeen":"2026-11-01T10:00:00Z","lastSeen":"2026-11-06T09:30:00Z"}`
	mux.HandleFunc("GET /api/0/organizations/acme/shortids/HERD-3P/", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"organizationSlug":"acme","groupId":"4504","shortId":"HERD-3P","group":` + group + `}`))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/issues/4504/{$}", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(group))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/issues/4504/events/recommended/", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("llmFormat") != "markdown" || r.URL.Query().Get("collapse") != "adjacentEvents" {
			t.Errorf("event query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(event))
	}))
	mux.HandleFunc("GET /api/0/organizations/acme/{$}", auth(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"slug":"acme"}`))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// cannedEvent carries user data in every place Sentry puts it: the user object,
// the tags, and the formatted markdown.
const cannedEvent = `{
  "id": "ev1",
  "user": {"email": "jane@example.com", "ip_address": "203.0.113.7", "username": "jane"},
  "tags": [{"key": "user", "value": "email:jane@example.com"}, {"key": "client_ip", "value": "203.0.113.7"}],
  "formatted": "# TypeError: x is undefined\n\n## Exception\n\nat secrets.load (secrets.ts:42)\n\n## User\n\n- email: jane@example.com\n- ip_address: 203.0.113.7\n\n## Tags\n\n- user: email:jane@example.com\n- release: herd@1.4.0\n- server: 2001:db8::1 at 10:20:30\n- contact: ops+alerts@acme.io\n"
}`

func TestFetchShortID(t *testing.T) {
	srv := fakeSentry(t, cannedEvent)
	c := Client{BaseURL: srv.URL, Org: "acme", Token: "tok", HTTP: srv.Client()}
	got, err := c.Fetch(Ref{ShortID: "HERD-3P"})
	if err != nil {
		t.Fatal(err)
	}
	want := Issue{
		ID: "4504", ShortID: "HERD-3P", Title: "TypeError: x is undefined", Culprit: "secrets.load",
		Permalink: "https://acme.sentry.io/issues/4504/", Level: "error", Status: "unresolved",
		Count: "128", UserCount: 17, FirstSeen: "2026-11-01T10:00:00Z", LastSeen: "2026-11-06T09:30:00Z",
	}
	if !strings.Contains(got.Event, "at secrets.load (secrets.ts:42)") {
		t.Errorf("event = %q, want the formatted markdown", got.Event)
	}
	got.Event = ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("issue = %+v\nwant  %+v", got, want)
	}
}

// Nothing Gather keeps carries the event's user data: not the user object, not a
// user tag, not an email or IP address anywhere in the formatted body (#211).
func TestGatherScrubsUserData(t *testing.T) {
	srv := fakeSentry(t, cannedEvent)
	c := Client{BaseURL: srv.URL, Org: "acme", Token: "tok", HTTP: srv.Client()}
	snap := Gather([]Ref{{ShortID: "HERD-3P"}}, c.Fetch)
	if len(snap.Issues) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	event := snap.Issues[0].Event
	for _, leak := range []string{"jane@example.com", "ops+alerts@acme.io", "203.0.113.7", "2001:db8::1", "jane", "## User"} {
		if strings.Contains(event, leak) {
			t.Errorf("event leaks %q:\n%s", leak, event)
		}
	}
	for _, kept := range []string{"at secrets.load (secrets.ts:42)", "release: herd@1.4.0", "10:20:30", "## Tags"} {
		if !strings.Contains(event, kept) {
			t.Errorf("event lost %q:\n%s", kept, event)
		}
	}
}

func TestFetchGroupID(t *testing.T) {
	srv := fakeSentry(t, cannedEvent)
	c := Client{BaseURL: srv.URL, Org: "acme", Token: "tok", HTTP: srv.Client()}
	got, err := c.Fetch(Ref{GroupID: "4504"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ShortID != "HERD-3P" || got.Event == "" {
		t.Errorf("issue = %+v", got)
	}
}

func TestFetchFailures(t *testing.T) {
	srv := fakeSentry(t, cannedEvent)
	cases := []struct {
		name string
		c    Client
		ref  Ref
		want string
	}{
		{"no token", Client{BaseURL: srv.URL, Org: "acme", HTTP: srv.Client()}, Ref{ShortID: "HERD-3P"}, "SENTRY_AUTH_TOKEN is not set"},
		{"a rejected token", Client{BaseURL: srv.URL, Org: "acme", Token: "bad", HTTP: srv.Client()}, Ref{ShortID: "HERD-3P"}, "HTTP 401"},
		{"an unknown short id", Client{BaseURL: srv.URL, Org: "acme", Token: "tok", HTTP: srv.Client()}, Ref{ShortID: "HERD-9"}, "HTTP 404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.Fetch(tc.ref)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestFetchKeepsTheIssueWhenTheEventFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/0/organizations/acme/issues/7/{$}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"7","shortId":"HERD-7","title":"boom"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := Client{BaseURL: srv.URL, Org: "acme", Token: "tok", HTTP: srv.Client()}
	got, err := c.Fetch(Ref{GroupID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "boom" || !strings.Contains(got.Event, "could not be fetched") || !strings.Contains(got.Event, "HTTP 404") {
		t.Errorf("issue = %+v", got)
	}
}

func TestGatherTruncatesTheEvent(t *testing.T) {
	long := strings.Repeat("é", EventBudget) // two bytes each
	snap := Gather([]Ref{{GroupID: "4504"}}, func(Ref) (Issue, error) { return Issue{ID: "4504", Event: long}, nil })
	got := snap.Issues[0]
	if !strings.Contains(got.Event, "truncated") {
		t.Errorf("no truncation marker in %d bytes", len(got.Event))
	}
	if len(got.Event) > EventBudget+200 {
		t.Errorf("event is %d bytes, budget %d", len(got.Event), EventBudget)
	}
	if !strings.HasPrefix(got.Event, "é") || strings.ContainsRune(got.Event, '\uFFFD') {
		t.Error("truncation split a rune")
	}
}

func TestGatherScrubsTheIssueFields(t *testing.T) {
	snap := Gather([]Ref{{GroupID: "8"}}, func(Ref) (Issue, error) {
		return Issue{ID: "8", ShortID: "HERD-8", Title: "login failed for bob@example.com from 198.51.100.4", Culprit: "auth(bob@example.com)"}, nil
	})
	got := snap.Issues[0]
	if strings.Contains(got.Title+got.Culprit, "bob@") || strings.Contains(got.Title, "198.51.100.4") {
		t.Errorf("issue fields leak PII: %+v", got)
	}
}

func TestPing(t *testing.T) {
	srv := fakeSentry(t, cannedEvent)
	if err := (Client{BaseURL: srv.URL, Org: "acme", Token: "tok", HTTP: srv.Client()}).Ping(); err != nil {
		t.Errorf("ping with a good token: %v", err)
	}
	if err := (Client{BaseURL: srv.URL, Org: "acme", Token: "bad", HTTP: srv.Client()}).Ping(); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("ping with a bad token: %v", err)
	}
}

// Sentry's own data scrubbing is configured per org, so the harness does not
// rely on it: credentials an event carries are redacted before the prompt, and
// the key that carried each one stays readable (#234 review).
func TestScrubRedactsCredentials(t *testing.T) {
	in := strings.Join([]string{
		"## Request",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.leak1",
		"- **Cookie**: session=leak2; csrftoken=leak3",
		"Set-Cookie: sid=leak4; Path=/",
		"x-api-key: leak5",
		`{"headers": {"authorization": "Bearer abcdefghijklmnopqrstuvwxyz.leak6"}}`,
		"GET /download?file=a.pdf&X-Amz-Signature=leak7&X-Amz-Credential=leak8",
		"GET /cb?access_token=leak9&state=keepme",
		`config: {"api_key": "leak10", "client_secret": 'leak11', "password": "leak12"}`,
		"csrf_token=leak13 sig=leak14 signature: leak15",
		"retrying with Bearer abcdefghijklmnopqrstuvwxyz0123leak16",
		"at secrets.load (secrets.ts:42)",
	}, "\n")
	out := Scrub(in)
	for i := 1; i <= 16; i++ {
		if leak := fmt.Sprintf("leak%d", i); strings.Contains(out, leak) {
			t.Errorf("Scrub leaks %s:\n%s", leak, out)
		}
	}
	for _, kept := range []string{"Authorization:", "Cookie", "X-Amz-Signature=", "access_token=", "state=keepme", `"api_key"`, "file=a.pdf", "at secrets.load (secrets.ts:42)", "## Request"} {
		if !strings.Contains(out, kept) {
			t.Errorf("Scrub lost %q:\n%s", kept, out)
		}
	}
}
