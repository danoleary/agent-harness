package ci

import "testing"

func TestParseChecksReadsGhJSON(t *testing.T) {
	// The shape `gh pr checks <branch> --json name,bucket,state,link` emits.
	raw := []byte(`[
		{"name":"build","bucket":"pass","state":"SUCCESS","link":"https://github.com/beherd/herd/actions/runs/100/job/1"},
		{"name":"lint","bucket":"fail","state":"FAILURE","link":"https://github.com/beherd/herd/actions/runs/100/job/2"}
	]`)
	checks, err := ParseChecks(raw)
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(checks))
	}
	if checks[0].Name != "build" || checks[0].Bucket != "pass" {
		t.Fatalf("checks[0] = %+v, want build/pass", checks[0])
	}
	if checks[1].Name != "lint" || checks[1].Bucket != "fail" {
		t.Fatalf("checks[1] = %+v, want lint/fail", checks[1])
	}
	if checks[1].Link == "" {
		t.Fatalf("checks[1].Link not populated")
	}
}

func TestParseChecksEmptyArray(t *testing.T) {
	checks, err := ParseChecks([]byte(`[]`))
	if err != nil {
		t.Fatalf("ParseChecks: %v", err)
	}
	if len(checks) != 0 {
		t.Fatalf("got %d checks, want 0", len(checks))
	}
}

func TestParseChecksRejectsGarbage(t *testing.T) {
	if _, err := ParseChecks([]byte(`not json`)); err == nil {
		t.Fatal("expected an error on non-JSON input")
	}
}
