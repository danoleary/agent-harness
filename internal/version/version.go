// Package version carries the harness's own version and the comparison a
// Consumer's `min_harness_version` pin is checked with.
//
// The version exists for that check. A Consumer commits its config against the
// harness it was written for, and an older binary reading a newer config does not
// fail — it ignores the keys it does not know, so an unknown gate is skipped and a
// renamed field reverts to a default while the run still reports success. The pin
// turns that into an error the operator reads at startup.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// DevVersion is what a build with no ldflags reports.
const DevVersion = "dev"

// Version is the harness version, set at release time with
// `-ldflags "-X github.com/danoleary/agent-harness/internal/version.Version=v1.2.3"`.
// It defaults to DevVersion so a plain `go build` never claims to be a release.
var Version = DevVersion

// IsDev reports whether v is an unversioned from-source build, which cannot be
// compared against a pin.
func IsDev(v string) bool { return v == DevVersion || v == "" }

// AtLeast reports whether actual is the same as, or later than, required. Both may
// carry a leading "v" and may omit components, which count as zero — so "0.2"
// means "0.2.0". Comparison is numeric per component, so 0.10 is above 0.9.
func AtLeast(actual, required string) (bool, error) {
	a, err := parse(actual)
	if err != nil {
		return false, fmt.Errorf("harness version %q: %w", actual, err)
	}
	r, err := parse(required)
	if err != nil {
		return false, fmt.Errorf("required version %q: %w", required, err)
	}
	for i := range a {
		if a[i] != r[i] {
			return a[i] > r[i], nil
		}
	}
	return true, nil
}

// parse turns "v0.2" into [0 2 0]. It is deliberately strict: a typo in a pin must
// fail loud, because a pin that silently does not parse disables the check it
// exists to make.
func parse(v string) ([3]int, error) {
	var out [3]int
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return out, fmt.Errorf("not a version (expected MAJOR.MINOR[.PATCH])")
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return out, fmt.Errorf("has %d components (expected MAJOR.MINOR[.PATCH])", len(parts))
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("component %q is not a number", p)
		}
		out[i] = n
	}
	return out, nil
}
