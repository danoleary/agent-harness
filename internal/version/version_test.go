package version

import (
	"strings"
	"testing"
)

// A Consumer's config is written against a harness version. Nothing used to
// record which, so an old binary reading a newer config silently ignored the keys
// it did not know: an unknown gate, a renamed tracker field or a missing
// `docs_only_excluded_roots` reverts to a default and the run looks healthy while
// doing the wrong thing. The comparison behind that check has to be boring and
// total.
func TestAtLeast(t *testing.T) {
	cases := []struct {
		actual   string
		required string
		want     bool
	}{
		{"0.2.0", "0.2", true},  // an equal version satisfies its own floor
		{"0.2.1", "0.2", true},  // a later patch satisfies it
		{"0.3.0", "0.2", true},  // a later minor satisfies it
		{"1.0.0", "0.9", true},  // a later major satisfies it
		{"0.1.0", "0.2", false}, // the case that used to fail silently
		{"0.2.0", "0.2.1", false},
		{"0.9.0", "1.0", false},
		// A leading v is how the tags are written, and an operator will paste one.
		{"v0.2.0", "v0.2", true},
		{"0.2.0", "v0.3", false},
		// Missing components are zero, so "0.2" means "0.2.0".
		{"0.2", "0.2.0", true},
		// Ordering is numeric, not lexical: 10 is above 9, and "0.10" is not "0.1".
		{"0.10.0", "0.9", true},
		{"0.9.0", "0.10", false},
	}
	for _, c := range cases {
		got, err := AtLeast(c.actual, c.required)
		if err != nil {
			t.Errorf("AtLeast(%q, %q) errored: %v", c.actual, c.required, err)
			continue
		}
		if got != c.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", c.actual, c.required, got, c.want)
		}
	}
}

// A malformed requirement must fail loud rather than silently pass: a typo in the
// pin would otherwise disable the very check the pin exists to make.
func TestAtLeastRejectsUnparseableVersions(t *testing.T) {
	for _, bad := range []string{"", "latest", "0.x", "one.two", "0.2.3.4"} {
		if _, err := AtLeast("0.2.0", bad); err == nil {
			t.Errorf("AtLeast with required=%q should error", bad)
		}
	}
}

// A from-source build carries no version, so there is nothing to compare. It must
// not block the maintainer working on the harness itself.
func TestIsDevBuild(t *testing.T) {
	if !IsDev("dev") {
		t.Error(`IsDev("dev") should be true`)
	}
	if IsDev("0.2.0") {
		t.Error(`IsDev("0.2.0") should be false`)
	}
}

// The default is a dev build: a binary built with plain `go build` (no ldflags)
// must not claim to be a release.
func TestVersionDefaultsToDev(t *testing.T) {
	if !strings.Contains(Version, "dev") {
		t.Errorf("Version = %q, want a dev default when no ldflags are set", Version)
	}
}
