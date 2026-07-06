package findings

import (
	"encoding/json"
	"regexp"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestParseEmptyDropbox(t *testing.T) {
	r := Parse("")
	if len(r.Findings) != 0 {
		t.Errorf("findings = %v, want empty", r.Findings)
	}
	if r.Error != "" {
		t.Errorf("error = %q, want none", r.Error)
	}
}

func TestParseWhitespaceOnly(t *testing.T) {
	r := Parse("  \n\t ")
	if len(r.Findings) != 0 || r.Error != "" {
		t.Errorf("whitespace dropbox should yield no findings, no error; got %+v", r)
	}
}

func TestParseValidArray(t *testing.T) {
	json := mustJSON(t, []any{
		map[string]any{"title": "tokens:build missing", "body": "Storybook died until I ran it", "kind": "setup"},
		map[string]any{"title": "opaque vitest error", "body": "bare file-list run crashes"},
	})

	r := Parse(json)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("findings len = %d, want 2", len(r.Findings))
	}
	want := Finding{Title: "tokens:build missing", Body: "Storybook died until I ran it", Kind: "setup"}
	if got := r.Findings[0]; got.Title != want.Title || got.Body != want.Body || got.Kind != want.Kind {
		t.Errorf("findings[0] = %+v, want %+v", got, want)
	}
	if r.Findings[1].Kind != "" {
		t.Errorf("findings[1].Kind = %q, want empty", r.Findings[1].Kind)
	}
}

func TestParseReadsOptionalKey(t *testing.T) {
	json := mustJSON(t, []any{
		map[string]any{"title": "Playwright missing", "body": "no deps", "key": "sandbox-playwright-missing-deps"},
		map[string]any{"title": "no key here", "body": "x"},
	})

	r := Parse(json)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if r.Findings[0].Key != "sandbox-playwright-missing-deps" {
		t.Errorf("findings[0].Key = %q, want the dedup key", r.Findings[0].Key)
	}
	if r.Findings[1].Key != "" {
		t.Errorf("findings[1].Key = %q, want empty", r.Findings[1].Key)
	}
}

func TestParseReadsAudience(t *testing.T) {
	json := mustJSON(t, []any{
		map[string]any{"title": "flaky spec", "body": "your test retries", "audience": "project"},
		map[string]any{"title": "sandbox missing deps", "body": "no libnss3", "audience": "harness"},
	})

	r := Parse(json)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if r.Findings[0].Audience != AudienceProject {
		t.Errorf("findings[0].Audience = %q, want %q", r.Findings[0].Audience, AudienceProject)
	}
	if r.Findings[1].Audience != AudienceHarness {
		t.Errorf("findings[1].Audience = %q, want %q", r.Findings[1].Audience, AudienceHarness)
	}
}

// An unclassified finding (no audience, or an unrecognized value) defaults to
// harness — the safe sink that keeps the Consumer's tracker clean of harness
// noise and never leaves the repo (ADR-0011).
func TestParseDefaultsUnclassifiedToHarness(t *testing.T) {
	json := mustJSON(t, []any{
		map[string]any{"title": "no audience", "body": "legacy dropbox"},
		map[string]any{"title": "bogus audience", "body": "typo", "audience": "elsewhere"},
	})

	r := Parse(json)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if r.Findings[0].Audience != AudienceHarness {
		t.Errorf("missing audience = %q, want default %q", r.Findings[0].Audience, AudienceHarness)
	}
	if r.Findings[1].Audience != AudienceHarness {
		t.Errorf("unrecognized audience = %q, want default %q", r.Findings[1].Audience, AudienceHarness)
	}
}

func TestParseMalformedJSON(t *testing.T) {
	r := Parse("{not json")
	if len(r.Findings) != 0 {
		t.Errorf("findings = %v, want empty", r.Findings)
	}
	if !regexp.MustCompile(`(?i)json`).MatchString(r.Error) {
		t.Errorf("error %q does not mention json", r.Error)
	}
}

func TestParseNonArray(t *testing.T) {
	r := Parse(mustJSON(t, map[string]any{"title": "x", "body": "y"}))
	if len(r.Findings) != 0 {
		t.Errorf("findings = %v, want empty", r.Findings)
	}
	if !regexp.MustCompile(`(?i)array`).MatchString(r.Error) {
		t.Errorf("error %q does not mention array", r.Error)
	}
}

func TestParseSkipsIncompleteEntries(t *testing.T) {
	json := mustJSON(t, []any{
		map[string]any{"title": "ok", "body": "valid"},
		map[string]any{"title": "no body"},
		"a string",
		map[string]any{"body": "no title"},
	})

	r := Parse(json)
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if len(r.Findings) != 1 || r.Findings[0].Title != "ok" || r.Findings[0].Body != "valid" {
		t.Errorf("findings = %+v, want [{ok valid}]", r.Findings)
	}
}
