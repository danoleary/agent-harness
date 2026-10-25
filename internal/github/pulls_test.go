package github

import (
	"reflect"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
)

// OpenPRs is the PR loop's poll (ADR-0019): one list read, mapped onto the fields
// the Source filters on. A PR whose head lives in another repository — or whose
// fork was deleted, leaving no head repo at all — is a fork the harness cannot push to.
func TestOpenPRsMapsTheFieldsTheSourceFiltersOn(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/pulls?state=open&per_page=100": []any{
			map[string]any{
				"number": 12, "title": "Add widget", "body": "Adds it", "html_url": "https://github.com/acme/widgets/pull/12",
				"head":   map[string]any{"ref": "feat/gh-7", "sha": "abc123", "repo": map[string]any{"full_name": "acme/widgets"}},
				"base":   map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/widgets"}},
				"labels": []any{map[string]any{"name": "agent-maintained"}},
			},
			map[string]any{
				"number": 13, "title": "From a fork",
				"head": map[string]any{"ref": "main", "sha": "def456", "repo": map[string]any{"full_name": "mallory/widgets"}},
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/widgets"}},
			},
			map[string]any{
				"number": 14, "title": "Deleted fork",
				"head": map[string]any{"ref": "x", "sha": "fff000", "repo": nil},
				"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/widgets"}},
			},
		},
	})
	got, err := NewPulls(tr, "acme/widgets").OpenPRs()
	if err != nil {
		t.Fatalf("OpenPRs: %v", err)
	}
	want := []PullRequest{
		{Number: 12, Title: "Add widget", Body: "Adds it", URL: "https://github.com/acme/widgets/pull/12", HeadBranch: "feat/gh-7", HeadSHA: "abc123", BaseBranch: "main", Labels: []string{"agent-maintained"}},
		{Number: 13, Title: "From a fork", HeadBranch: "main", HeadSHA: "def456", BaseBranch: "main", Fork: true},
		{Number: 14, Title: "Deleted fork", HeadBranch: "x", HeadSHA: "fff000", BaseBranch: "main", Fork: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OpenPRs =\n%+v\nwant\n%+v", got, want)
	}
}

// The REST pull carries mergeability as a nullable boolean plus a lower-case state;
// both reduce through ci.ClassifyMergeState, so the loop and the CI watch can never
// disagree on what "conflicting" means.
func TestMergeStateReducesThroughTheCIClassifier(t *testing.T) {
	cases := []struct {
		name  string
		pull  map[string]any
		wantV ci.MergeVerdict
	}{
		{"dirty", map[string]any{"mergeable": false, "mergeable_state": "dirty"}, ci.MergeConflicting},
		{"clean", map[string]any{"mergeable": true, "mergeable_state": "clean"}, ci.MergeClean},
		{"behind is not a conflict", map[string]any{"mergeable": true, "mergeable_state": "behind"}, ci.MergeClean},
		{"still computing", map[string]any{"mergeable": nil, "mergeable_state": "unknown"}, ci.MergeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, _ := fakeTransport(map[string]any{"GET /repos/acme/widgets/pulls/12": c.pull})
			got, err := NewPulls(tr, "acme/widgets").MergeState(12)
			if err != nil {
				t.Fatalf("MergeState: %v", err)
			}
			if got != c.wantV {
				t.Errorf("MergeState = %v, want %v", got, c.wantV)
			}
		})
	}
}

// Checks folds the head commit's check runs and its legacy commit statuses into one
// verdict: any failure is red (even while others still run, so the loop need not
// wait for a verdict it already has), anything unfinished is pending, else green.
func TestChecksFoldsCheckRunsAndStatuses(t *testing.T) {
	run := func(status, conclusion string) map[string]any {
		return map[string]any{"name": "ci", "status": status, "conclusion": conclusion}
	}
	cases := []struct {
		name   string
		runs   []any
		status map[string]any
		want   ChecksVerdict
	}{
		{"all green", []any{run("completed", "success"), run("completed", "skipped")}, map[string]any{"state": "pending", "total_count": 0}, ChecksGreen},
		{"a failed run", []any{run("completed", "failure"), run("in_progress", "")}, map[string]any{"state": "pending", "total_count": 0}, ChecksRed},
		{"a timed-out run", []any{run("completed", "timed_out")}, map[string]any{"state": "pending", "total_count": 0}, ChecksRed},
		{"still running", []any{run("completed", "success"), run("queued", "")}, map[string]any{"state": "pending", "total_count": 0}, ChecksPending},
		{"a failed status", []any{run("completed", "success")}, map[string]any{"state": "failure", "total_count": 1}, ChecksRed},
		{"a pending status", []any{run("completed", "success")}, map[string]any{"state": "pending", "total_count": 1}, ChecksPending},
		{"no checks at all", []any{}, map[string]any{"state": "pending", "total_count": 0}, ChecksGreen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, _ := fakeTransport(map[string]any{
				"GET /repos/acme/widgets/commits/abc123/check-runs?per_page=100": map[string]any{"check_runs": c.runs},
				"GET /repos/acme/widgets/commits/abc123/status":                  c.status,
			})
			got, err := NewPulls(tr, "acme/widgets").Checks("abc123")
			if err != nil {
				t.Fatalf("Checks: %v", err)
			}
			if got != c.want {
				t.Errorf("Checks = %v, want %v", got, c.want)
			}
		})
	}
}

// Comments, labels: the PR loop's lease and its attempt record live on the PR, so
// they ride the issues endpoints every PR also answers to.
func TestCommentsAndLabelsUseTheIssueEndpoints(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/issues/12/comments?per_page=100": []any{
			map[string]any{"body": "first", "created_at": "2026-10-20T10:00:00Z"},
		},
		"POST /repos/acme/widgets/issues/12/comments":               map[string]any{},
		"POST /repos/acme/widgets/issues/12/labels":                 []any{},
		"DELETE /repos/acme/widgets/issues/12/labels/in%20progress": []any{},
	})
	p := NewPulls(tr, "acme/widgets")

	comments, err := p.Comments(12)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if want := []PRComment{{Body: "first", CreatedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}}; !reflect.DeepEqual(comments, want) {
		t.Errorf("Comments = %+v, want %+v", comments, want)
	}
	if err := p.Comment(12, "hello"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := p.AddLabel(12, "blocked"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if err := p.RemoveLabel(12, "in progress"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	got := (*calls)[1:]
	if !reflect.DeepEqual(got[0].body, map[string]any{"body": "hello"}) {
		t.Errorf("comment body = %v", got[0].body)
	}
	if !reflect.DeepEqual(got[1].body, map[string]any{"labels": []string{"blocked"}}) {
		t.Errorf("label body = %v", got[1].body)
	}
}
