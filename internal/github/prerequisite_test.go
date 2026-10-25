package github

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// openPR / mergedPR / openIssue are the issues-API shapes a body reference can
// resolve to: an open pull request still carrying its unmerged head branch, a pull
// request that has since merged (closed), and a plain open issue.
var (
	openPR    = map[string]any{"number": 12, "title": "Design record", "state": "open", "pull_request": map[string]any{"url": "u"}}
	mergedPR  = map[string]any{"number": 12, "title": "Design record", "state": "closed", "pull_request": map[string]any{"url": "u"}}
	openIssue = map[string]any{"number": 12, "title": "Some issue", "state": "open"}
)

func readyWithBody(n int, body string) map[string]any {
	return map[string]any{"number": n, "title": "work", "body": body, "labels": []any{map[string]any{"name": "ready-for-agent"}}}
}

// #82: a ticket whose body names a still-open PR builds on a base that is about
// to change underneath it, so selection leaves it in the queue — the same way the
// native blocked-by skip holds a ticket behind an open issue — and moves on.
func TestSelectNextTicketHoldsATicketWhoseBodyNamesAnOpenPR(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		selectPath: []any{
			readyWithBody(9, "Step one of the design in #12."),
			readyWithBody(10, "independent"),
		},
		"GET /repos/acme/widgets/issues/12": openPR,
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)
	var log bytes.Buffer
	c.log = &log

	got, ok, err := c.SelectNextTicket()
	if err != nil || !ok {
		t.Fatalf("SelectNextTicket = ok %v, err %v", ok, err)
	}
	if got.Identifier != "#10" {
		t.Errorf("selected %s, want #10 — #9 names the still-open PR #12", got.Identifier)
	}
	if !strings.Contains(log.String(), "#9 skipped: builds on open PR #12") {
		t.Errorf("log = %q, want the skip narrated", log.String())
	}
}

// Only an OPEN PULL REQUEST holds a ticket: a merged prerequisite is on main
// already, and a plain open issue named in prose is a mention, not an unmerged
// branch (a declared blocked-by relation is how an issue blocks). A reference that
// no longer resolves holds nothing either, so a stale number cannot wedge the queue.
func TestSelectNextTicketIgnoresReferencesThatAreNotOpenPRs(t *testing.T) {
	for name, ref := range map[string]any{"merged PR": mergedPR, "open issue": openIssue, "missing": nil} {
		responses := map[string]any{selectPath: []any{readyWithBody(9, "See #12 and https://github.com/acme/widgets/pull/12.")}}
		if ref != nil {
			responses["GET /repos/acme/widgets/issues/12"] = ref
		}
		tr, _ := fakeTransport(responses)
		if ref == nil {
			tr = notFoundFor(tr, "/repos/acme/widgets/issues/12")
		}
		c := NewClient(tr, "acme", "widgets", readyLabels)

		got, ok, err := c.SelectNextTicket()
		if err != nil || !ok || got.Identifier != "#9" {
			t.Errorf("%s: SelectNextTicket = %s ok %v err %v, want #9", name, got.Identifier, ok, err)
		}
	}
}

// A failed read of a referenced item is not a free pass (fail closed, like the
// native blocker read).
func TestSelectNextTicketFailsClosedOnAnUnreadableReference(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{selectPath: []any{readyWithBody(9, "builds on #12")}})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	if _, _, err := c.SelectNextTicket(); err == nil {
		t.Fatal("an unreadable reference must surface as an error, not an unblocked ticket")
	}
}

// What counts as a reference: a bare `#N` and a same-repo issue/PR URL. A `#N`
// glued to a word, a URL fragment, an HTML entity, another repo's URL, and the
// ticket's own number are not.
func TestReferencedNumbers(t *testing.T) {
	body := "Builds on #12 (see https://github.com/acme/widgets/pull/13).\n" +
		"#14 first. Not foo#15, not /path#16, not &#17;, not https://github.com/other/repo/pull/18, not #9, again #12."
	got := referencedNumbers(body, "acme", "widgets", 9)
	want := []int{12, 13, 14}
	if len(got) != len(want) {
		t.Fatalf("referencedNumbers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("referencedNumbers = %v, want %v", got, want)
		}
	}
}

// When the ticket is dispatched anyway (hand-passed, or the PR opened after
// selection), FetchTicket names each open prerequisite PR and its head branch so
// the implementation envelope can tell the session where the design lives.
func TestFetchTicketNamesOpenPrerequisitePRs(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/issues/9":  readyWithBody(9, "Step one of #12; unrelated to #13."),
		"GET /repos/acme/widgets/issues/12": openPR,
		"GET /repos/acme/widgets/pulls/12": map[string]any{
			"number": 12, "title": "Design record", "html_url": "https://github.com/acme/widgets/pull/12",
			"head": map[string]any{"ref": "feat/gh-11"},
		},
		"GET /repos/acme/widgets/issues/13": openIssue,
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	got, err := c.FetchTicket("#9")
	if err != nil {
		t.Fatalf("FetchTicket: %v", err)
	}
	if len(got.Prerequisites) != 1 {
		t.Fatalf("Prerequisites = %+v, want only the open PR #12", got.Prerequisites)
	}
	p := got.Prerequisites[0]
	if p.Identifier != "#12" || p.Branch != "feat/gh-11" || p.Title != "Design record" || p.URL != "https://github.com/acme/widgets/pull/12" {
		t.Errorf("prerequisite = %+v", p)
	}
}

// notFoundFor wraps a transport so a GET of path answers GitHub's 404.
func notFoundFor(tr Transport, path string) Transport {
	return func(method, p string, body any) (json.RawMessage, error) {
		if method == "GET" && p == path {
			return nil, httpError(404, []byte(`{"message":"Not Found"}`))
		}
		return tr(method, p, body)
	}
}
