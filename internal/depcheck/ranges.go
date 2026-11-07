package depcheck

import (
	"fmt"
	"strings"
)

// Range is one GitHub advisory vulnerable range: comparators that a version
// must all satisfy, such as `>= 1.0.0, < 1.2.3`.
type Range []Comparator

// Comparator is one operator and version, such as `< 1.2.3`.
type Comparator struct {
	Op      string // one of "=", "<", "<=", ">", ">="
	Version Version
}

// ParseRange parses GitHub's range syntax: comma-separated comparators, each an
// operator (`=`, `<`, `<=`, `>`, `>=`, or none for `=`) and a version.
func ParseRange(s string) (Range, error) {
	var r Range
	for _, part := range strings.Split(s, ",") {
		c, err := parseComparator(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("range %q: %w", s, err)
		}
		r = append(r, c)
	}
	return r, nil
}

func parseComparator(s string) (Comparator, error) {
	if s == "" {
		return Comparator{}, fmt.Errorf("empty comparator")
	}
	op := "="
	for _, o := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(s, o) {
			op, s = o, strings.TrimSpace(s[len(o):])
			break
		}
	}
	v, err := parseVersion(s, true)
	if err != nil {
		return Comparator{}, err
	}
	return Comparator{Op: op, Version: v}, nil
}

// Contains reports whether v satisfies every comparator in r. A pre-release is
// ordered as semver orders it, with no npm-style exclusion: a vulnerable range
// that a pre-release falls inside contains it.
func (r Range) Contains(v Version) bool {
	for _, c := range r {
		if !c.matches(Compare(v, c.Version)) {
			return false
		}
	}
	return true
}

func (c Comparator) matches(cmp int) bool {
	switch c.Op {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	}
	return cmp == 0
}

func (c Comparator) String() string { return c.Op + " " + c.Version.String() }

func (r Range) String() string {
	parts := make([]string, len(r))
	for i, c := range r {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}
