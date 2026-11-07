// Package depcheck answers one question on the host: does a pnpm lockfile still
// resolve a version of a package inside any of a set of vulnerable ranges? It is
// the security-alerts loop's Ground truth check on a SecurityFix session's
// result: the session's self-report is not trusted (ADR-0016, ADR-0040).
//
// It reads GitHub's advisory range syntax and the `packages:` and `snapshots:`
// keys of a pnpm v6 or v9 lockfile line by line, with no YAML dependency.
package depcheck

import "fmt"

// Vulnerable returns the versions of pkg that lock resolves inside any of
// ranges, in ascending order. An empty result means the lockfile is clean for
// pkg. It fails closed, as Resolved does.
func Vulnerable(lock []byte, pkg string, ranges []Range) ([]Version, error) {
	resolved, err := Resolved(lock, pkg)
	if err != nil {
		return nil, err
	}
	var out []Version
	for _, v := range resolved {
		for _, r := range ranges {
			if r.Contains(v) {
				out = append(out, v)
				break
			}
		}
	}
	return out, nil
}

// Result is what a lockfile scan found for one package against its alerts'
// vulnerable ranges: the SecurityFix Ground truth's lockfile half
// (verify.SecurityFix). Err is set when the scan could not run: an unreadable
// lockfile, a range that would not parse, no range at all.
type Result struct {
	Package    string
	Vulnerable []Version
	Err        error
}

// Clean reports whether the scan ran and found no vulnerable version. The zero
// Result is no scan, so it is not clean.
func (r Result) Clean() bool {
	return r.Package != "" && r.Err == nil && len(r.Vulnerable) == 0
}

// Check scans lock for pkg against ranges, written in GitHub's syntax as the
// alerts carry them. Every way the scan can fail is folded into Err, so a caller
// cannot read a failed scan as a fixed lockfile.
func Check(lock []byte, pkg string, ranges []string) Result {
	r := Result{Package: pkg}
	if len(ranges) == 0 {
		r.Err = fmt.Errorf("no vulnerable range to check %s against", pkg)
		return r
	}
	parsed := make([]Range, 0, len(ranges))
	for _, s := range ranges {
		rg, err := ParseRange(s)
		if err != nil {
			r.Err = err
			return r
		}
		parsed = append(parsed, rg)
	}
	r.Vulnerable, r.Err = Vulnerable(lock, pkg, parsed)
	return r
}
