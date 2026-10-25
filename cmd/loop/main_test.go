package main

import "testing"

// `loop --prs` runs the PR loop and `loop --prs --dry-run` only prints its plan.
// A bare --dry-run is refused rather than ignored: ignoring it would start a real
// ticket loop when the operator asked for a dry run.
func TestParseFlags(t *testing.T) {
	cases := []struct {
		argv    []string
		want    flags
		wantErr bool
	}{
		{argv: nil, want: flags{}},
		{argv: []string{"--prs"}, want: flags{prs: true}},
		{argv: []string{"--prs", "--dry-run"}, want: flags{prs: true, dryRun: true}},
		{argv: []string{"--dry-run", "--prs", "--verbose"}, want: flags{prs: true, dryRun: true}},
		{argv: []string{"--dry-run"}, wantErr: true},
	}
	for _, c := range cases {
		got, err := parseFlags(c.argv)
		if (err != nil) != c.wantErr {
			t.Errorf("parseFlags(%v) error = %v, wantErr %v", c.argv, err, c.wantErr)
			continue
		}
		if err == nil && got != c.want {
			t.Errorf("parseFlags(%v) = %+v, want %+v", c.argv, got, c.want)
		}
	}
}
