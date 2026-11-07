package depcheck

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a semver 2.0 version. Build metadata is kept for String but plays
// no part in ordering.
type Version struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

// ParseVersion parses a full semver version, MAJOR.MINOR.PATCH with an optional
// pre-release and build. A leading "v" is allowed.
func ParseVersion(s string) (Version, error) {
	return parseVersion(s, false)
}

// parseVersion parses s; short allows "1" and "1.2", read as "1.0.0" and
// "1.2.0", which GitHub writes in some advisory ranges.
func parseVersion(s string, short bool) (Version, error) {
	var v Version
	rest := strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		v.Build = rest[i+1:]
		rest = rest[:i]
		if !validIdents(v.Build) {
			return Version{}, fmt.Errorf("version %q: malformed build metadata", s)
		}
	}
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		pre := rest[i+1:]
		rest = rest[:i]
		if !validIdents(pre) {
			return Version{}, fmt.Errorf("version %q: malformed pre-release", s)
		}
		v.Pre = strings.Split(pre, ".")
	}
	parts := strings.Split(rest, ".")
	if len(parts) > 3 || (len(parts) < 3 && !short) {
		return Version{}, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH", s)
	}
	nums := []*uint64{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		if !isNumeric(p) {
			return Version{}, fmt.Errorf("version %q: %q is not a number", s, p)
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: %w", s, err)
		}
		*nums[i] = n
	}
	return v, nil
}

// validIdents reports whether s is a non-empty dot-separated list of non-empty
// [0-9A-Za-z-] identifiers.
func validIdents(s string) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-') {
				return false
			}
		}
	}
	return true
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		s += "-" + strings.Join(v.Pre, ".")
	}
	if v.Build != "" {
		s += "+" + v.Build
	}
	return s
}

// Compare returns -1, 0 or 1 as a sorts before, with or after b, by semver 2.0
// precedence (§11).
func Compare(a, b Version) int {
	for _, d := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	// A pre-release sorts before its release.
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a.Pre), len(b.Pre))
}

// compareIdent orders two pre-release identifiers: numeric ones numerically (by
// length then digits, so no size overflows), numeric before alphanumeric, and
// alphanumeric ones in ASCII order.
func compareIdent(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if c := cmpInt(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
