// Package depcheck answers one question on the host: does a pnpm lockfile still
// resolve a version of a package inside any of a set of vulnerable ranges? It is
// the security-alerts loop's Ground truth check on a SecurityFix session's
// result: the session's self-report is not trusted (ADR-0016, ADR-0040).
//
// It reads GitHub's advisory range syntax and the `packages:` and `snapshots:`
// keys of a pnpm v6 or v9 lockfile line by line, with no YAML dependency.
package depcheck

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
